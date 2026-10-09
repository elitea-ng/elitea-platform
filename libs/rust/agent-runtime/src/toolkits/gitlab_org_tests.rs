//! Focused contract tests for the capability-disabled GitLab Org family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::{CONTENT_TYPE, HeaderName, HeaderValue};
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::gitlab_org::client::{
    GitLabOrgApi, GitLabOrgClient, GitLabOrgClientError, GitLabOrgClientErrorCode,
    GitLabOrgHttpResponse, GitLabOrgOperation, GitLabOrgTransport, test_map_http_status,
    test_parse_next_page, test_validate_effect_status,
};
use super::families::gitlab_org::config::{GitLabOrgConfigErrorCode, GitLabOrgToolkitConfig};
use super::families::gitlab_org::diff::{
    DiffErrorCode, test_discussion_position, test_format_changes,
};
use super::families::gitlab_org::edit::{EditErrorCode, test_apply_update};
use super::families::gitlab_org::tools::{
    GitLabOrgToolsetErrorCode, build_gitlab_org_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const PRIVATE_TOKEN: HeaderName = HeaderName::from_static("private-token");

fn settings(repositories: Option<&str>, selected: &[&str]) -> Map<String, Value> {
    let mut value = json!({
        "gitlab_configuration":{
            "url":"https://gitlab.example.test",
            "private_token":"private-token-value"
        },
        "branch":"main",
        "selected_tools":selected
    })
    .as_object()
    .cloned()
    .expect("GitLab Org fixture is an object");
    if let Some(repositories) = repositories {
        value.insert(
            "repositories".to_owned(),
            Value::String(repositories.to_owned()),
        );
    }
    value
}

fn config(repositories: Option<&str>, selected: &[&str]) -> GitLabOrgToolkitConfig {
    GitLabOrgToolkitConfig::parse(&settings(repositories, selected))
        .expect("valid GitLab Org fixture")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("GitLab Org policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("gitlab-org-test").with_function_call_id("gitlab-org-call"))
}

#[test]
fn catalog_preserves_source_order_and_groups() {
    assert_eq!(
        test_catalog(),
        vec![
            ("create_branch", "write"),
            ("set_active_branch", "write"),
            ("list_branches_in_repo", "read"),
            ("get_issues", "read"),
            ("get_issue", "read"),
            ("create_pull_request", "write"),
            ("comment_on_issue", "write"),
            ("create_file", "write"),
            ("read_file", "read"),
            ("update_file", "write"),
            ("delete_file", "delete"),
            ("get_pr_changes", "read"),
            ("create_pr_change_comment", "write"),
            ("list_files", "read"),
            ("list_folders", "read"),
            ("append_file", "write"),
            ("get_commits", "read"),
        ]
    );
}

#[test]
fn missing_and_empty_repositories_preserve_dynamic_mode_without_leaking_secret() {
    for repositories in [None, Some("")] {
        let parsed = GitLabOrgToolkitConfig::parse(&settings(repositories, &[]))
            .expect("empty dynamic repository mode parses without I/O");
        assert!(parsed.selected_tools().is_empty());
    }

    let mut invalid = settings(Some("group/project"), &[]);
    invalid
        .get_mut("gitlab_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested config")
        .insert(
            "url".to_owned(),
            Value::String("http://not-allowed.test".to_owned()),
        );
    let Err(error) = GitLabOrgToolkitConfig::parse(&invalid) else {
        panic!("production HTTP is rejected");
    };
    assert!(!format!("{error:?}").contains("private-token-value"));
}

#[derive(Default)]
struct FixtureApi {
    calls: Mutex<Vec<String>>,
}

#[async_trait]
impl GitLabOrgApi for FixtureApi {
    async fn execute(
        &self,
        operation: GitLabOrgOperation<'_>,
    ) -> Result<Value, GitLabOrgClientError> {
        let name = match operation {
            GitLabOrgOperation::CreateBranch { .. } => "create_branch",
            GitLabOrgOperation::SetActiveBranch { .. } => "set_active_branch",
            GitLabOrgOperation::ListBranches { .. } => "list_branches_in_repo",
            GitLabOrgOperation::GetIssues { .. } => "get_issues",
            GitLabOrgOperation::GetIssue { .. } => "get_issue",
            GitLabOrgOperation::CreatePullRequest { .. } => "create_pull_request",
            GitLabOrgOperation::CommentOnIssue { .. } => "comment_on_issue",
            GitLabOrgOperation::CreateFile { .. } => "create_file",
            GitLabOrgOperation::ReadFile { .. } => "read_file",
            GitLabOrgOperation::UpdateFile { .. } => "update_file",
            GitLabOrgOperation::DeleteFile { .. } => "delete_file",
            GitLabOrgOperation::GetPrChanges { .. } => "get_pr_changes",
            GitLabOrgOperation::CreatePrChangeComment { line_number, .. } => {
                assert_eq!(line_number, 0, "zero-based first diff row reaches the API");
                "create_pr_change_comment"
            }
            GitLabOrgOperation::ListFiles { .. } => "list_files",
            GitLabOrgOperation::ListFolders { .. } => "list_folders",
            GitLabOrgOperation::AppendFile { .. } => "append_file",
            GitLabOrgOperation::GetCommits { .. } => "get_commits",
        };
        self.calls
            .lock()
            .expect("GitLab Org calls")
            .push(name.to_owned());
        Ok(json!({"tool":name}))
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keeps the exact 17-tool model contract together.
async fn complete_catalog_schemas_descriptions_selection_and_policy_are_exact() {
    let api = Arc::new(FixtureApi::default());
    let api_trait: Arc<dyn GitLabOrgApi> = api.clone();
    let toolset = test_build_with_api("org-code", &[], &policy(&[]), &api_trait)
        .expect("complete GitLab Org toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("GitLab Org tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        test_catalog()
            .iter()
            .map(|(name, _)| *name)
            .collect::<Vec<_>>()
    );
    for (tool, (_, group)) in tools.iter().zip(test_catalog()) {
        assert_eq!(tool.is_read_only(), group == "read");
        assert!(!tool.is_concurrency_safe());
        assert!(tool.description().starts_with("Toolkit: org-code\n"));
        assert!(tool.description().len() <= 2_000);
        let schema = tool.parameters_schema().expect("GitLab Org schema");
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["additionalProperties"], false);
        for property in schema["properties"]
            .as_object()
            .expect("schema properties")
            .values()
        {
            assert!(
                property["description"]
                    .as_str()
                    .is_some_and(|value| !value.is_empty())
            );
        }
        for forbidden in ["gitlab.example.test", "private-token-value", "team/allowed"] {
            assert!(!tool.description().contains(forbidden));
            assert!(!schema.to_string().contains(forbidden));
        }
    }
    let schemas = tools
        .iter()
        .map(|tool| tool.parameters_schema().expect("schema"))
        .collect::<Vec<_>>();
    assert_eq!(schemas[0]["required"], json!(["branch_name"]));
    assert_eq!(schemas[2]["properties"]["limit"]["default"], 20);
    assert_eq!(schemas[8]["properties"]["start_line"]["minimum"], 1);
    assert_eq!(schemas[12]["properties"]["line_number"]["minimum"], 0);
    assert_eq!(schemas[13]["title"], "ListFoldersModel");
    assert_eq!(schemas[14]["title"], "ListFoldersModel");
    assert_eq!(schemas[13], schemas[14]);
    assert!(tools[5].description().contains("configured base branch"));
    assert!(tools[6].description().contains("positive issue iid"));
    assert!(tools[8].description().contains("200000"));
    assert!(tools[9].description().contains("OLD/NEW"));
    assert!(tools[12].description().contains("zero-based"));
    assert!(tools[16].description().contains("RFC3339"));

    tools[12]
        .execute(
            context(),
            json!({"pr_number":"7","file_path":"src/lib.rs","line_number":0,"comment":"first row","repository":null}),
        )
        .await
        .expect("zero-based comment invocation");

    let selected = vec![
        "get_commits".to_owned(),
        "get_issue".to_owned(),
        "get_issue".to_owned(),
    ];
    let subset = test_build_with_api("org-code", &selected, &policy(&[]), &api_trait)
        .expect("selected toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        subset
            .tools(readonly)
            .await
            .expect("selected tools")
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["get_issue", "get_commits"]
    );

    let blocked = test_build_with_api(
        "org-code",
        &[],
        &policy(&[("gitlab_org", &["get_issue"])]),
        &api_trait,
    )
    .expect("policy-filtered toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("filtered tools")
            .iter()
            .all(|tool| tool.name() != "get_issue")
    );

    let unknown = config(Some("team/allowed"), &["unknown_tool"]);
    let Err(error) = build_gitlab_org_toolset("org-code", unknown, &policy(&[])) else {
        panic!("unknown selection fails closed");
    };
    assert_eq!(
        error.code(),
        GitLabOrgToolsetErrorCode::UnsupportedSelection
    );
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct CapturedRequest {
    method: Method,
    url: String,
    body: Option<Value>,
    token: String,
    token_sensitive: bool,
    content_type: Option<String>,
    effect: bool,
}

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    responses: Mutex<VecDeque<Result<GitLabOrgHttpResponse, GitLabOrgClientError>>>,
}

impl FixtureTransport {
    fn new(responses: impl IntoIterator<Item = GitLabOrgHttpResponse>) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().map(Ok).collect()),
        }
    }

    fn requests(&self) -> Vec<CapturedRequest> {
        self.requests.lock().expect("GitLab Org requests").clone()
    }
}

#[async_trait]
impl GitLabOrgTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
        let body = request
            .body()
            .and_then(reqwest::Body::as_bytes)
            .and_then(|bytes| serde_json::from_slice(bytes).ok());
        let token = request
            .headers()
            .get(&PRIVATE_TOKEN)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        self.requests
            .lock()
            .expect("GitLab Org requests")
            .push(CapturedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                body,
                token,
                token_sensitive: request
                    .headers()
                    .get(&PRIVATE_TOKEN)
                    .is_some_and(reqwest::header::HeaderValue::is_sensitive),
                content_type: request
                    .headers()
                    .get(CONTENT_TYPE)
                    .and_then(|value| value.to_str().ok())
                    .map(ToOwned::to_owned),
                effect,
            });
        self.responses
            .lock()
            .expect("GitLab Org responses")
            .pop_front()
            .expect("fixture response for every request")
    }
}

fn client_with(
    repositories: Option<&str>,
    responses: impl IntoIterator<Item = GitLabOrgHttpResponse>,
) -> (GitLabOrgClient, Arc<FixtureTransport>) {
    let transport = Arc::new(FixtureTransport::new(responses));
    let transport_trait: Arc<dyn GitLabOrgTransport> = transport.clone();
    (
        GitLabOrgClient::fixture(config(repositories, &[]), transport_trait),
        transport,
    )
}

#[tokio::test]
async fn exact_routes_private_token_repository_trimming_state_and_fnmatch_are_bounded() {
    let branch_listing = json!([
        {"name":"release/]"},
        {"name":"release/a"},
        {"name":"feature/x"}
    ]);
    let (client, transport) = client_with(
        Some(" team/allowed ; team/other "),
        [
            GitLabOrgHttpResponse::fixture(
                StatusCode::CREATED,
                Some(json!({"name":"feature/login"})),
                None,
            ),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(branch_listing.clone()), None),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(branch_listing), None),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(json!([])), None),
        ],
    );

    assert_eq!(
        client
            .execute(GitLabOrgOperation::CreateBranch {
                branch_name: "feature/login",
                repository: Some("  team/allowed  "),
            })
            .await
            .expect("create branch"),
        Value::String("Branch feature/login created successfully and set as active".to_owned())
    );
    client
        .execute(GitLabOrgOperation::SetActiveBranch {
            branch: "release/1.2",
        })
        .await
        .expect("local active branch update");
    assert_eq!(
        transport.requests().len(),
        1,
        "local state change sends no HTTP"
    );
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ListBranches {
                repository: None,
                limit: 20,
                branch_wildcard: Some("release/[!]]"),
            })
            .await
            .expect("negated literal-closing-bracket fnmatch"),
        json!(["release/a"])
    );
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ListBranches {
                repository: None,
                limit: 20,
                branch_wildcard: Some("release/[]]"),
            })
            .await
            .expect("literal closing bracket fnmatch"),
        json!(["release/]"])
    );
    client
        .execute(GitLabOrgOperation::ListFiles {
            path: "",
            recursive: true,
            branch: None,
            repository: None,
        })
        .await
        .expect("active-branch tree listing");

    let requests = transport.requests();
    assert_eq!(requests[0].method, Method::POST);
    assert_eq!(
        requests[0].url,
        "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/branches"
    );
    assert_eq!(
        requests[0].body,
        Some(json!({"branch":"feature/login","ref":"main"}))
    );
    assert_eq!(requests[0].token, "private-token-value");
    assert!(requests[0].token_sensitive);
    assert_eq!(
        requests[0].content_type.as_deref(),
        Some("application/json")
    );
    assert!(requests[0].effect);
    assert_eq!(
        requests[1].url,
        "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/branches?per_page=100&page=1"
    );
    assert_eq!(
        requests[3].url,
        "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/tree?ref=release%2F1.2&recursive=true&per_page=100&page=1"
    );
}

#[tokio::test]
async fn dynamic_repository_mode_requires_and_encodes_an_explicit_trimmed_project() {
    let (client, transport) = client_with(
        None,
        [GitLabOrgHttpResponse::fixture(
            StatusCode::OK,
            Some(json!([])),
            None,
        )],
    );
    let error = client
        .execute(GitLabOrgOperation::GetIssues { repository: None })
        .await
        .expect_err("dynamic mode cannot infer a repository");
    assert_eq!(error.code(), GitLabOrgClientErrorCode::InvalidConfiguration);
    client
        .execute(GitLabOrgOperation::GetIssues {
            repository: Some("  org/project  "),
        })
        .await
        .expect("explicit organization-wide project");
    assert_eq!(
        transport.requests()[0].url,
        "https://gitlab.example.test/api/v4/projects/org%2Fproject/issues?state=opened&per_page=20&page=1"
    );

    let (allowlisted, allowlist_transport) = client_with(
        Some("team/allowed"),
        std::iter::empty::<GitLabOrgHttpResponse>(),
    );
    assert_eq!(
        allowlisted
            .execute(GitLabOrgOperation::GetIssues {
                repository: Some("team/other"),
            })
            .await
            .expect_err("non-allowlisted project fails before HTTP")
            .code(),
        GitLabOrgClientErrorCode::Authorization
    );
    assert!(allowlist_transport.requests().is_empty());

    for repository in [
        None,
        Some("project"),
        Some("../group/project"),
        Some("group/../project"),
        Some("group/\0project"),
    ] {
        let (dynamic, dynamic_transport) =
            client_with(None, std::iter::empty::<GitLabOrgHttpResponse>());
        assert!(
            dynamic
                .execute(GitLabOrgOperation::GetIssues { repository })
                .await
                .is_err()
        );
        assert!(dynamic_transport.requests().is_empty());
    }
}

#[test]
fn transient_read_statuses_and_dispatched_effect_statuses_have_safe_retry_contracts() {
    for (status, code) in [
        (
            StatusCode::REQUEST_TIMEOUT,
            GitLabOrgClientErrorCode::Timeout,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            GitLabOrgClientErrorCode::RateLimited,
        ),
        (
            StatusCode::BAD_GATEWAY,
            GitLabOrgClientErrorCode::DependencyUnavailable,
        ),
    ] {
        let error = test_map_http_status(status, false);
        assert_eq!(error.code(), code);
        assert!(error.retryable());
    }
    for status in [
        StatusCode::REQUEST_TIMEOUT,
        StatusCode::TOO_MANY_REQUESTS,
        StatusCode::BAD_GATEWAY,
    ] {
        let error = test_map_http_status(status, true);
        assert_eq!(error.code(), GitLabOrgClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
}

#[test]
fn pagination_header_presence_and_effect_confirmation_statuses_fail_closed() {
    assert_eq!(test_parse_next_page(None).expect("missing header"), None);
    assert_eq!(
        test_parse_next_page(Some(&HeaderValue::from_static(""))).expect("empty terminal header"),
        None
    );
    assert_eq!(
        test_parse_next_page(Some(&HeaderValue::from_static("10")))
            .expect("positive decimal continuation")
            .as_deref(),
        Some("10")
    );
    for malformed in ["0", "-1", "next", "123456789012345678901"] {
        let value = HeaderValue::from_str(malformed).expect("ASCII header fixture");
        assert_eq!(
            test_parse_next_page(Some(&value))
                .expect_err("present malformed pagination header fails")
                .code(),
            GitLabOrgClientErrorCode::InvalidResponse
        );
    }
    let non_utf8 = HeaderValue::from_bytes(&[0xff]).expect("opaque non-UTF8 header fixture");
    assert_eq!(
        test_parse_next_page(Some(&non_utf8))
            .expect_err("present non-UTF8 pagination header fails")
            .code(),
        GitLabOrgClientErrorCode::InvalidResponse
    );

    for status in [
        StatusCode::OK,
        StatusCode::ACCEPTED,
        StatusCode::PARTIAL_CONTENT,
    ] {
        let error = test_validate_effect_status(&Method::POST, status)
            .expect_err("POST effects require exact 201");
        assert_eq!(error.code(), GitLabOrgClientErrorCode::UnknownOutcome);
        assert!(!error.retryable());
    }
    test_validate_effect_status(&Method::POST, StatusCode::CREATED)
        .expect("all GitLab POST effects confirm only on 201");
    test_validate_effect_status(&Method::DELETE, StatusCode::NO_CONTENT)
        .expect("GitLab DELETE confirms only on 204");
    assert_eq!(
        test_validate_effect_status(&Method::DELETE, StatusCode::OK)
            .expect_err("DELETE 200 is ambiguous")
            .code(),
        GitLabOrgClientErrorCode::UnknownOutcome
    );
}

fn provider_file(content: &str) -> GitLabOrgHttpResponse {
    GitLabOrgHttpResponse::fixture(
        StatusCode::OK,
        Some(json!({
            "content":STANDARD.encode(content.as_bytes()),
            "last_commit_id":"commit-1"
        })),
        None,
    )
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One boundary corpus makes read-limit interactions explicit.
async fn file_reads_enforce_character_serialized_utf8_source_and_range_bounds() {
    let accepted = "a".repeat(200_000);
    let (client, _) = client_with(Some("team/allowed"), [provider_file(&accepted)]);
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ReadFile {
                file_path: "large.txt",
                branch: "main",
                repository: None,
                start_line: None,
                end_line: None,
            })
            .await
            .expect("200000 characters fit"),
        Value::String(accepted)
    );

    let too_many = "a".repeat(200_001);
    let (client, _) = client_with(Some("team/allowed"), [provider_file(&too_many)]);
    let guidance = client
        .execute(GitLabOrgOperation::ReadFile {
            file_path: "single.txt",
            branch: "main",
            repository: None,
            start_line: None,
            end_line: None,
        })
        .await
        .expect("oversized single-line guidance");
    assert_eq!(guidance["__result_status__"], "content_too_large");
    assert_eq!(guidance["context"]["actual_chars"], 200_001);
    assert_eq!(
        guidance["instruction_for_readFile"]["first_class_params"],
        json!({})
    );
    assert_eq!(guidance["read_limits"]["max_serialized_bytes"], 512 * 1_024);
    assert!(
        guidance["instruction_for_readFile"]["notes"]
            .as_str()
            .is_some_and(|value| value.contains("character result limit")
                && !value.contains("serialized result limit"))
    );

    let multibyte_single = "😀".repeat(140_000);
    let (client, _) = client_with(Some("team/allowed"), [provider_file(&multibyte_single)]);
    let guidance = client
        .execute(GitLabOrgOperation::ReadFile {
            file_path: "emoji.txt",
            branch: "main",
            repository: None,
            start_line: None,
            end_line: None,
        })
        .await
        .expect("serialized output bound becomes guidance");
    assert_eq!(guidance["context"]["actual_chars"], 140_000);
    assert!(
        guidance["context"]["actual_serialized_bytes"]
            .as_u64()
            .is_some_and(|value| value > 512 * 1_024)
    );
    assert_eq!(
        guidance["instruction_for_readFile"]["first_class_params"],
        json!({})
    );
    assert!(
        guidance["instruction_for_readFile"]["notes"]
            .as_str()
            .is_some_and(|value| value.contains("serialized result limit")
                && !value.contains("character result limit"))
    );

    let multibyte_lines = format!("{}\n{}", "😀".repeat(70_000), "😀".repeat(70_000));
    let (client, _) = client_with(Some("team/allowed"), [provider_file(&multibyte_lines)]);
    let guidance = client
        .execute(GitLabOrgOperation::ReadFile {
            file_path: "emoji-lines.txt",
            branch: "main",
            repository: None,
            start_line: None,
            end_line: None,
        })
        .await
        .expect("multiline serialized guidance");
    assert!(guidance["instruction_for_readFile"]["first_class_params"]["start_line"].is_string());

    let (client, _) = client_with(Some("team/allowed"), [provider_file("one\ntwo\nthree")]);
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ReadFile {
                file_path: "ranges.txt",
                branch: "main",
                repository: None,
                start_line: Some(2),
                end_line: Some(2),
            })
            .await
            .expect("inclusive line range"),
        Value::String("two\n".to_owned())
    );
    let (client, _) = client_with(Some("team/allowed"), [provider_file("one\ntwo")]);
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ReadFile {
                file_path: "ranges.txt",
                branch: "main",
                repository: None,
                start_line: Some(3),
                end_line: Some(3),
            })
            .await
            .expect_err("past-end line range fails")
            .code(),
        GitLabOrgClientErrorCode::InvalidInput
    );

    let over_source = "x".repeat(1_024 * 1_024 + 1);
    let (client, _) = client_with(Some("team/allowed"), [provider_file(&over_source)]);
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ReadFile {
                file_path: "too-large.txt",
                branch: "main",
                repository: None,
                start_line: None,
                end_line: None,
            })
            .await
            .expect_err("decoded source cap")
            .code(),
        GitLabOrgClientErrorCode::ResourceExhausted
    );
}

#[tokio::test]
async fn issue_projection_preserves_nullable_body_and_empty_file_creation_is_exact() {
    let (client, _) = client_with(
        Some("team/allowed"),
        [
            GitLabOrgHttpResponse::fixture(
                StatusCode::OK,
                Some(json!({"title":"Broken","description":null})),
                None,
            ),
            GitLabOrgHttpResponse::fixture(
                StatusCode::OK,
                Some(json!([{"body":"note","author":{"username":"alice"}}])),
                None,
            ),
        ],
    );
    assert_eq!(
        client
            .execute(GitLabOrgOperation::GetIssue {
                issue_number: 9,
                repository: None
            })
            .await
            .expect("issue projection"),
        json!({"title":"Broken","body":null,"comments":[{"body":"note","user":"alice"}]})
    );

    let (client, transport) = client_with(
        Some("team/allowed"),
        [
            GitLabOrgHttpResponse::fixture(
                StatusCode::NOT_FOUND,
                Some(json!({"message":"404"})),
                None,
            ),
            GitLabOrgHttpResponse::fixture(
                StatusCode::CREATED,
                Some(json!({"file_path":"empty.txt"})),
                None,
            ),
        ],
    );
    client
        .execute(GitLabOrgOperation::CreateFile {
            file_path: "empty.txt",
            contents: "",
            branch: "main",
            repository: None,
        })
        .await
        .expect("empty file creation");
    let requests = transport.requests();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].method, Method::GET);
    assert_eq!(requests[1].method, Method::POST);
    assert_eq!(
        requests[1].body.as_ref().expect("create body")["content"],
        ""
    );
}

#[test]
fn old_new_parser_and_diff_positions_are_deterministic_and_bounded() {
    let query = "OLD<<<<\nlet  value = 1;\n>>>>OLD\nNEW<<<<\nlet value = 2;\n>>>>NEW";
    assert_eq!(
        test_apply_update("src/lib.rs", "fn main() {\n  let\tvalue = 1;\n}\n", query)
            .expect("unique whitespace-tolerant edit"),
        "fn main() {\nlet value = 2;\n}\n"
    );
    assert_eq!(
        test_apply_update(
            "image.png",
            "old",
            "OLD<<<<\nold\n>>>>OLD\nNEW<<<<\nnew\n>>>>NEW"
        )
        .expect_err("extension allowlist"),
        EditErrorCode::UnsupportedFile
    );
    let dense = "alpha   beta\n".repeat(70_000);
    assert_eq!(
        test_apply_update(
            "dense.txt",
            &dense,
            "OLD<<<<\nalpha beta\n>>>>OLD\nNEW<<<<\nx\n>>>>NEW"
        )
        .expect_err("near-bound duplicate tolerant match"),
        EditErrorCode::Ambiguous
    );

    let merge_request = json!({
        "title":"Change",
        "description":null,
        "diff_refs":{"base_sha":"base","head_sha":"head","start_sha":"start"}
    });
    let changes = json!({"changes":[{
        "old_path":"src/lib.rs",
        "new_path":"src/lib.rs",
        "diff":"@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file"
    }]});
    let formatted = test_format_changes(&merge_request, &changes).expect("count-less unified hunk");
    assert!(formatted.starts_with("title: Change\ndescription: None\n\n"));
    assert!(formatted.contains("0:@@ -1 +1 @@"));
    let position = test_discussion_position(&merge_request, &changes, "src/lib.rs", 0)
        .expect("zero-based hunk-header position");
    assert_eq!(position["old_line"], 1);
    assert_eq!(position["old_path"], "src/lib.rs");
    assert_eq!(position["base_sha"], "base");

    let huge_metadata = json!({"title":"x".repeat(512 * 1_024),"description":""});
    assert_eq!(
        test_format_changes(&huge_metadata, &json!({"changes":[]}))
            .expect_err("metadata-only output is bounded"),
        DiffErrorCode::ResourceExhausted
    );
    let dense_diff = format!("@@ -1,1 +1,1 @@\n {}", "x".repeat(512 * 1_024));
    assert_eq!(
        test_format_changes(
            &json!({"title":"x","description":"y"}),
            &json!({"changes":[{"old_path":"a","new_path":"b","diff":dense_diff}]})
        )
        .expect_err("dense near-bound diff is bounded"),
        DiffErrorCode::ResourceExhausted
    );
}

#[test]
fn configuration_bounds_defaults_and_secret_debug_contract_are_strict() {
    let parsed = config(
        Some("group/a; group/a, group/b"),
        &["get_issue", "get_issue"],
    );
    assert_eq!(parsed.selected_tools(), [Box::<str>::from("get_issue")]);
    let mut missing_branch = settings(Some("group/project"), &[]);
    missing_branch.remove("branch");
    GitLabOrgToolkitConfig::parse(&missing_branch).expect("missing branch defaults to main");

    for bad_url in [
        "http://gitlab.test",
        "https://user@gitlab.test",
        "https://gitlab.test/path",
        "https://gitlab.test?query=1",
        "https://gitlab.test#fragment",
    ] {
        let mut invalid = settings(Some("group/project"), &[]);
        invalid["gitlab_configuration"]["url"] = Value::String(bad_url.to_owned());
        let Err(error) = GitLabOrgToolkitConfig::parse(&invalid) else {
            panic!("invalid origin must fail");
        };
        assert_eq!(error.code(), GitLabOrgConfigErrorCode::InvalidConfiguration);
        assert!(!format!("{error:?}").contains("private-token-value"));
    }
    let oversized = "x".repeat(1_025);
    let error = GitLabOrgToolkitConfig::parse(&settings(Some(&oversized), &[]))
        .err()
        .expect("configured repository envelope cap");
    assert_eq!(error.code(), GitLabOrgConfigErrorCode::ResourceExhausted);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered corpus proves every multi-request effect wire contract.
async fn remote_effect_routes_queries_bodies_and_confirmation_statuses_are_exact() {
    let merge_request = json!({
        "title":"MR",
        "description":"body",
        "diff_refs":{"base_sha":"base","head_sha":"head","start_sha":"start"}
    });
    let changes = json!({"changes":[{
        "old_path":"src/a b.rs",
        "new_path":"src/a b.rs",
        "diff":"@@ -1 +1 @@\n-old\n+new"
    }]});
    let (client, transport) = client_with(
        Some("team/allowed"),
        [
            GitLabOrgHttpResponse::fixture(StatusCode::CREATED, Some(json!({"iid":8})), None),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(json!({"iid":42})), None),
            GitLabOrgHttpResponse::fixture(StatusCode::CREATED, Some(json!({"id":1})), None),
            provider_file("old"),
            GitLabOrgHttpResponse::fixture(
                StatusCode::CREATED,
                Some(json!({"id":"commit-2"})),
                None,
            ),
            provider_file("old"),
            GitLabOrgHttpResponse::fixture(StatusCode::NO_CONTENT, None, None),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(merge_request), None),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(changes), None),
            GitLabOrgHttpResponse::fixture(
                StatusCode::CREATED,
                Some(json!({"id":"discussion"})),
                None,
            ),
            provider_file("old"),
            GitLabOrgHttpResponse::fixture(
                StatusCode::CREATED,
                Some(json!({"id":"commit-3"})),
                None,
            ),
        ],
    );

    client
        .execute(GitLabOrgOperation::CreatePullRequest {
            title: "Feature",
            body: "Line one\nLine two",
            branch: "feature/x",
            repository: None,
        })
        .await
        .expect("merge request");
    client
        .execute(GitLabOrgOperation::CommentOnIssue {
            issue_number: 42,
            comment: "First\nSecond",
            repository: None,
        })
        .await
        .expect("issue note");
    client
        .execute(GitLabOrgOperation::UpdateFile {
            file_path: "src/a b.rs",
            update_query: "OLD<<<<\nold\n>>>>OLD\nNEW<<<<\nnew\n>>>>NEW",
            branch: "feature/x",
            repository: None,
        })
        .await
        .expect("file update");
    client
        .execute(GitLabOrgOperation::DeleteFile {
            file_path: "src/a b.rs",
            branch: "feature/x",
            repository: None,
        })
        .await
        .expect("file deletion");
    client
        .execute(GitLabOrgOperation::CreatePrChangeComment {
            pr_number: 7,
            file_path: "src/a b.rs",
            line_number: 0,
            comment: "Review\nthis",
            repository: None,
        })
        .await
        .expect("diff discussion");
    client
        .execute(GitLabOrgOperation::AppendFile {
            file_path: "src/a b.rs",
            content: "tail\nline",
            branch: "feature/x",
            repository: None,
        })
        .await
        .expect("file append");

    let requests = transport.requests();
    assert_eq!(requests.len(), 12);
    let expected = [
        (
            Method::POST,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/merge_requests",
            true,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/issues/42",
            false,
        ),
        (
            Method::POST,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/issues/42/notes",
            true,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/files/src%2Fa%20b.rs?ref=feature%2Fx",
            false,
        ),
        (
            Method::POST,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/commits",
            true,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/files/src%2Fa%20b.rs?ref=feature%2Fx",
            false,
        ),
        (
            Method::DELETE,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/files/src%2Fa%20b.rs?branch=feature%2Fx&commit_message=Delete+src%2Fa+b.rs&last_commit_id=commit-1",
            true,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/merge_requests/7",
            false,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/merge_requests/7/changes",
            false,
        ),
        (
            Method::POST,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/merge_requests/7/discussions",
            true,
        ),
        (
            Method::GET,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/files/src%2Fa%20b.rs?ref=feature%2Fx",
            false,
        ),
        (
            Method::POST,
            "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/commits",
            true,
        ),
    ];
    for (request, (method, url, effect)) in requests.iter().zip(expected) {
        assert_eq!(request.method, method);
        assert_eq!(request.url, url);
        assert_eq!(request.effect, effect);
        assert!(request.token_sensitive);
    }
    assert_eq!(
        requests[0].body,
        Some(json!({
            "source_branch":"feature/x","target_branch":"main","title":"Feature",
            "description":"Line one\nLine two","labels":["created-by-agent"]
        }))
    );
    assert_eq!(requests[2].body, Some(json!({"body":"First\nSecond"})));
    assert_eq!(
        requests[4].body.as_ref().expect("update body")["actions"][0],
        json!({
            "action":"update","file_path":"src/a b.rs","content":"new","last_commit_id":"commit-1"
        })
    );
    assert_eq!(
        requests[9].body.as_ref().expect("discussion body")["position"],
        json!({
            "old_line":1,"old_path":"src/a b.rs","base_sha":"base","head_sha":"head",
            "start_sha":"start","position_type":"text"
        })
    );
    assert_eq!(
        requests[11].body.as_ref().expect("append body")["actions"][0],
        json!({
            "action":"update","file_path":"src/a b.rs","content":"old\ntail\nline","last_commit_id":"commit-1"
        })
    );
    assert_eq!(
        requests[11].body.as_ref().expect("append body")["commit_message"],
        "Append src/a b.rs"
    );
}

#[tokio::test]
async fn commits_filters_and_pagination_are_exact_and_exhaustion_is_explicit() {
    let commit = json!({
        "id":"abc","author_name":"Alice","created_at":"2025-01-02T03:04:05Z",
        "message":"change","web_url":"https://gitlab.example.test/commit/abc"
    });
    let (client, transport) = client_with(
        Some("team/allowed"),
        [
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(json!([commit])), Some("2")),
            GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(json!([])), None),
        ],
    );
    assert_eq!(
        client
            .execute(GitLabOrgOperation::GetCommits {
                sha: Some("main"),
                path: Some("src/lib.rs"),
                since: Some("2025-01-01T00:00:00Z"),
                until: Some("2025-02-01T00:00:00+00:00"),
                author: Some("Alice"),
                repository: None,
            })
            .await
            .expect("paged commits"),
        json!([{"sha":"abc","author":"Alice","createdAt":"2025-01-02T03:04:05Z","message":"change","url":"https://gitlab.example.test/commit/abc"}])
    );
    assert_eq!(
        transport.requests()[0].url,
        "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/commits?ref_name=main&path=src%2Flib.rs&since=2025-01-01T00%3A00%3A00Z&until=2025-02-01T00%3A00%3A00%2B00%3A00&author=Alice&per_page=100&page=1"
    );
    assert_eq!(
        transport.requests()[1].url,
        "https://gitlab.example.test/api/v4/projects/team%2Fallowed/repository/commits?ref_name=main&path=src%2Flib.rs&since=2025-01-01T00%3A00%3A00Z&until=2025-02-01T00%3A00%3A00%2B00%3A00&author=Alice&per_page=100&page=2"
    );

    let pages = (1..=10)
        .map(|page| {
            GitLabOrgHttpResponse::fixture(
                StatusCode::OK,
                Some(json!([])),
                Some(&(page + 1).to_string()),
            )
        })
        .collect::<Vec<_>>();
    let (client, transport) = client_with(Some("team/allowed"), pages);
    assert_eq!(
        client
            .execute(GitLabOrgOperation::ListBranches {
                repository: None,
                limit: 20,
                branch_wildcard: None,
            })
            .await
            .expect_err("ten-page traversal refuses continuation")
            .code(),
        GitLabOrgClientErrorCode::ResourceExhausted
    );
    assert_eq!(transport.requests().len(), 10);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn GitLabOrgApi> = Arc::new(FixtureApi::default());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete gitlab_org toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("gitlab_org tools");
    super::sdk_conformance::assert_sdk_conformance("gitlab_org", &tools);
}

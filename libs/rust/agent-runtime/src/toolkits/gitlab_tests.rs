//! Contract tests for the single-project GitLab family (SDK `gitlab`).

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use reqwest::header::HeaderName;
use reqwest::{Method, Request, StatusCode};
use serde_json::{Map, Value, json};

use super::families::gitlab::client::{
    GitLabApi, GitLabClient, GitLabClientError, GitLabClientErrorCode, GitLabOperation,
};
use super::families::gitlab::config::{GitLabConfigErrorCode, GitLabToolkitConfig};
use super::families::gitlab::tools::{GitLabToolsetErrorCode, test_build_with_api, test_catalog};
use super::families::gitlab_org::client::{
    GitLabOrgClientError, GitLabOrgHttpResponse, GitLabOrgTransport,
};
use super::policy::ToolAdmissionPolicy;

const PRIVATE_TOKEN: HeaderName = HeaderName::from_static("private-token");
const PROJECT: &str = "https://gitlab.example.test/api/v4/projects/team%2Fproject";

fn settings(selected: &[&str]) -> Map<String, Value> {
    json!({
        "gitlab_configuration": {
            "url": "https://gitlab.example.test/",
            "private_token": "private-token-value"
        },
        "repository": "team/project/",
        "branch": "main",
        "selected_tools": selected,
        "pgvector_configuration": null,
        "embedding_model": null
    })
    .as_object()
    .cloned()
    .expect("GitLab fixture is an object")
}

fn config() -> GitLabToolkitConfig {
    GitLabToolkitConfig::parse(&settings(&[])).expect("valid GitLab fixture")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("GitLab policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("gitlab-test").with_function_call_id("gitlab-call"))
}

#[test]
fn catalog_follows_the_sdk_order_without_index_tools() {
    assert_eq!(
        test_catalog(),
        vec![
            ("create_branch", "write"),
            ("delete_branch", "delete"),
            ("list_branches_in_repo", "read"),
            ("list_files", "read"),
            ("list_folders", "read"),
            ("get_issues", "read"),
            ("get_issue", "read"),
            ("create_pull_request", "write"),
            ("comment_on_issue", "write"),
            ("comment_on_pr", "write"),
            ("create_file", "write"),
            ("read_file", "read"),
            ("update_file", "write"),
            ("append_file", "write"),
            ("delete_file", "delete"),
            ("set_active_branch", "write"),
            ("get_pr_changes", "read"),
            ("create_pr_change_comment", "write"),
            ("get_commits", "read"),
            ("read_multiple_files", "read"),
            ("grep_file", "read"),
        ]
    );
}

#[test]
fn configuration_is_strict_and_never_leaks_the_token() {
    let parsed = config();
    assert!(parsed.selected_tools().is_empty());

    let mut http = settings(&[]);
    http["gitlab_configuration"]["url"] = json!("http://gitlab.example.test");
    let Err(error) = GitLabToolkitConfig::parse(&http) else {
        panic!("plain HTTP is refused");
    };
    assert_eq!(error.code(), GitLabConfigErrorCode::InvalidConfiguration);
    assert!(!format!("{error:?}").contains("private-token-value"));

    let mut missing = settings(&[]);
    missing.remove("repository");
    assert!(GitLabToolkitConfig::parse(&missing).is_err());

    let mut traversal = settings(&[]);
    traversal.insert("repository".to_owned(), json!("team/../other"));
    assert!(GitLabToolkitConfig::parse(&traversal).is_err());

    let mut default_branch = settings(&[]);
    default_branch.remove("branch");
    assert!(GitLabToolkitConfig::parse(&default_branch).is_ok());
}

#[derive(Default)]
struct FixtureApi {
    calls: Mutex<Vec<String>>,
}

#[async_trait]
impl GitLabApi for FixtureApi {
    async fn execute(&self, operation: GitLabOperation<'_>) -> Result<Value, GitLabClientError> {
        let name = match operation {
            GitLabOperation::ListBranches { limit, .. } => format!("list_branches:{limit}"),
            GitLabOperation::CreatePrChangeComment { line_number, .. } => {
                format!("create_pr_change_comment:{line_number}")
            }
            GitLabOperation::ReadMultipleFiles { file_paths, .. } => {
                format!("read_multiple_files:{}", file_paths.join(","))
            }
            GitLabOperation::GrepFile {
                is_regex,
                context_lines,
                ..
            } => format!("grep_file:{is_regex}:{context_lines}"),
            _ => "other".to_owned(),
        };
        self.calls.lock().expect("calls").push(name.clone());
        Ok(json!({"call": name}))
    }
}

async fn tools_for(
    api: &Arc<FixtureApi>,
    selected: &[&str],
) -> Result<Vec<Arc<dyn adk_core::Tool>>, GitLabToolsetErrorCode> {
    let api_trait: Arc<dyn GitLabApi> = api.clone();
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(*name))
        .collect::<Vec<_>>();
    let toolset = test_build_with_api("code", &selected, &policy(&[]), &api_trait)
        .map_err(|error| error.code())?;
    let readonly: Arc<dyn ReadonlyContext> = context();
    Ok(toolset.tools(readonly).await.expect("GitLab tools"))
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered story over shared fixture state.
async fn schemas_selection_and_argument_shapes_follow_the_sdk() {
    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &[]).await.expect("complete toolset");
    assert_eq!(tools.len(), 21);
    for (tool, (name, group)) in tools.iter().zip(test_catalog()) {
        assert_eq!(tool.name(), name);
        assert_eq!(tool.is_read_only(), group == "read");
        assert!(!tool.is_concurrency_safe());
        assert!(tool.description().starts_with("Toolkit: code\n"));
        assert!(tool.description().len() <= 1_000);
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["additionalProperties"], false);
        for forbidden in ["gitlab.example.test", "private-token-value", "team/project"] {
            assert!(!tool.description().contains(forbidden));
            assert!(!schema.to_string().contains(forbidden));
        }
    }

    let by_name = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .cloned()
            .expect("tool")
    };
    // An omitted limit is the SDK default 20; the policy wrapper drops an
    // explicit null as BaseAction does, so it is the default too.
    by_name("list_branches_in_repo")
        .execute(context(), json!({}))
        .await
        .expect("default limit");
    by_name("list_branches_in_repo")
        .execute(context(), json!({"limit": null}))
        .await
        .expect("null limit");
    by_name("create_pr_change_comment")
        .execute(
            context(),
            json!({"pr_number": "7", "file_path": "a.py", "line_number": 0, "comment": "c"}),
        )
        .await
        .expect("lax integer and zero line index");
    by_name("read_multiple_files")
        .execute(context(), json!({"file_paths": ["a.py", "b.py"]}))
        .await
        .expect("batch read");
    by_name("grep_file")
        .execute(context(), json!({"file_path": "a.py", "pattern": "x"}))
        .await
        .expect("grep defaults");
    assert_eq!(
        *api.calls.lock().expect("calls"),
        [
            "list_branches:20",
            "list_branches:20",
            "create_pr_change_comment:0",
            "read_multiple_files:a.py,b.py",
            "grep_file:true:2"
        ]
    );
    for (name, arguments) in [
        ("get_issues", json!({"unexpected": 1})),
        ("get_issue", json!({"issue_number": 0})),
        ("list_branches_in_repo", json!({"limit": 0})),
        ("read_multiple_files", json!({"file_paths": []})),
        (
            "grep_file",
            json!({"file_path": "a", "pattern": "x", "context_lines": 99}),
        ),
    ] {
        assert!(
            by_name(name).execute(context(), arguments).await.is_err(),
            "{name} refuses a malformed call"
        );
    }

    // A known index tool in a selection is dropped, not fatal; an unknown
    // name and an index-only selection are refused.
    let subset = tools_for(&api, &["read_file", "index_data", "read_file"])
        .await
        .expect("index tool dropped");
    assert_eq!(
        subset.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["read_file"]
    );
    assert_eq!(
        tools_for(&api, &["search_index"]).await.err(),
        Some(GitLabToolsetErrorCode::UnsupportedSelection)
    );
    assert_eq!(
        tools_for(&api, &["no_such_tool"]).await.err(),
        Some(GitLabToolsetErrorCode::UnsupportedSelection)
    );

    let api_trait: Arc<dyn GitLabApi> = api.clone();
    let blocked = test_build_with_api(
        "code",
        &[],
        &policy(&[("gitlab", &["delete_branch"])]),
        &api_trait,
    )
    .expect("policy filtered");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("tools")
            .iter()
            .all(|tool| tool.name() != "delete_branch")
    );
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &[]).await.expect("complete toolset");
    super::sdk_conformance::assert_sdk_conformance("gitlab", &tools);
}

#[derive(Clone, Debug)]
struct CapturedRequest {
    method: Method,
    url: String,
    body: Option<Value>,
    token: String,
    token_sensitive: bool,
    effect: bool,
}

struct FixtureTransport {
    requests: Mutex<Vec<CapturedRequest>>,
    responses: Mutex<VecDeque<GitLabOrgHttpResponse>>,
}

#[async_trait]
impl GitLabOrgTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<GitLabOrgHttpResponse, GitLabOrgClientError> {
        let header = request.headers().get(&PRIVATE_TOKEN);
        self.requests
            .lock()
            .expect("requests")
            .push(CapturedRequest {
                method: request.method().clone(),
                url: request.url().to_string(),
                body: request
                    .body()
                    .and_then(reqwest::Body::as_bytes)
                    .and_then(|bytes| serde_json::from_slice(bytes).ok()),
                token: header
                    .and_then(|value| value.to_str().ok())
                    .unwrap_or_default()
                    .to_owned(),
                token_sensitive: header.is_some_and(reqwest::header::HeaderValue::is_sensitive),
                effect,
            });
        Ok(self
            .responses
            .lock()
            .expect("responses")
            .pop_front()
            .expect("a fixture response for every request"))
    }
}

fn ok(body: Value) -> GitLabOrgHttpResponse {
    GitLabOrgHttpResponse::fixture(StatusCode::OK, Some(body), None)
}

fn created(body: Value) -> GitLabOrgHttpResponse {
    GitLabOrgHttpResponse::fixture(StatusCode::CREATED, Some(body), None)
}

fn status(code: StatusCode) -> GitLabOrgHttpResponse {
    GitLabOrgHttpResponse::fixture(code, None, None)
}

/// A files-API answer as GitLab sends it (base64 content).
fn file(content: &str) -> GitLabOrgHttpResponse {
    ok(json!({
        "file_name": "x",
        "file_path": "x",
        "encoding": "base64",
        "content": STANDARD.encode(content),
        "last_commit_id": "abc123",
        "ref": "feature"
    }))
}

fn client(
    responses: impl IntoIterator<Item = GitLabOrgHttpResponse>,
) -> (GitLabClient, Arc<FixtureTransport>) {
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        responses: Mutex::new(responses.into_iter().collect()),
    });
    let transport_trait: Arc<dyn GitLabOrgTransport> = transport.clone();
    (GitLabClient::fixture(config(), transport_trait), transport)
}

fn text(value: &Value) -> &str {
    value.as_str().expect("a text result")
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered story over shared fixture state.
async fn branches_follow_the_active_branch_and_protection_rules() {
    let (client, transport) = client([
        created(json!({"name": "feature/a"})),
        GitLabOrgHttpResponse::fixture(
            StatusCode::BAD_REQUEST,
            Some(json!({"message": "Branch already exists"})),
            None,
        ),
        ok(json!([{"type": "blob", "path": "a.py"}, {"type": "tree", "path": "src"}])),
        status(StatusCode::NO_CONTENT),
        status(StatusCode::NO_CONTENT),
    ]);
    let created = client
        .execute(GitLabOperation::CreateBranch {
            branch_name: "feature/a",
        })
        .await
        .expect("create");
    assert_eq!(
        text(&created),
        "Branch feature/a created successfully and set as active"
    );
    let existing = client
        .execute(GitLabOperation::CreateBranch {
            branch_name: "feature/b",
        })
        .await
        .expect("already exists");
    assert_eq!(
        text(&existing),
        "Branch feature/b already exists. set it as active"
    );
    let files = client
        .execute(GitLabOperation::ListFiles {
            path: None,
            recursive: true,
            branch: None,
        })
        .await
        .expect("files");
    assert_eq!(files, json!(["a.py"]));

    for branch in ["MAIN", "master"] {
        let refused = client
            .execute(GitLabOperation::DeleteBranch {
                branch_name: branch,
                force: true,
            })
            .await
            .expect("protected");
        assert!(text(&refused).starts_with(&format!("Cannot delete protected branch '{branch}'")));
    }
    let active = client
        .execute(GitLabOperation::DeleteBranch {
            branch_name: "feature/b",
            force: false,
        })
        .await
        .expect("active refused");
    assert_eq!(
        text(&active),
        "Cannot delete active branch 'feature/b'. Either switch branches first or use force=True parameter."
    );
    let forced = client
        .execute(GitLabOperation::DeleteBranch {
            branch_name: "feature/b",
            force: true,
        })
        .await
        .expect("forced");
    assert_eq!(
        text(&forced),
        "Successfully deleted branch 'feature/b' and reset active branch to 'main'"
    );
    let other = client
        .execute(GitLabOperation::DeleteBranch {
            branch_name: "feature/a",
            force: false,
        })
        .await
        .expect("other");
    assert_eq!(text(&other), "Successfully deleted branch 'feature/a'");

    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(requests.len(), 5);
    assert_eq!(requests[0].method, Method::POST);
    assert_eq!(requests[0].url, format!("{PROJECT}/repository/branches"));
    assert_eq!(
        requests[0].body,
        Some(json!({"branch": "feature/a", "ref": "main"}))
    );
    assert_eq!(requests[0].token, "private-token-value");
    assert!(requests[0].token_sensitive);
    assert!(requests[0].effect);
    // The second branch is cut from the first, which became active.
    assert_eq!(
        requests[1].body,
        Some(json!({"branch": "feature/b", "ref": "feature/a"}))
    );
    assert_eq!(
        requests[2].url,
        format!("{PROJECT}/repository/tree?recursive=true&ref=feature%2Fb&per_page=100&page=1")
    );
    assert_eq!(requests[3].method, Method::DELETE);
    assert_eq!(
        requests[3].url,
        format!("{PROJECT}/repository/branches/feature%2Fb")
    );
}

#[tokio::test]
async fn listings_issues_and_commits_keep_the_sdk_shapes() {
    let (client, transport) = client([
        GitLabOrgHttpResponse::fixture(
            StatusCode::OK,
            Some(json!([{"name": "main"}, {"name": "release/1"}])),
            Some("2"),
        ),
        ok(json!([{"name": "release/2"}, {"name": "dev"}])),
        ok(json!([{"title": "It's broken", "iid": 4}])),
        ok(json!([])),
        ok(json!({"title": "T", "description": null})),
        ok(json!([{"body": "hi", "author": {"username": "ann"}}])),
        ok(json!([{
            "id": "c1", "author_name": "Ann", "created_at": "2025-01-01T00:00:00Z",
            "message": "fix", "web_url": "https://gitlab.example.test/c1"
        }])),
    ]);
    let branches = client
        .execute(GitLabOperation::ListBranches {
            limit: 20,
            branch_wildcard: Some("release/*"),
        })
        .await
        .expect("branches");
    assert_eq!(branches, json!(["release/1", "release/2"]));
    let issues = client
        .execute(GitLabOperation::GetIssues)
        .await
        .expect("issues");
    assert_eq!(
        text(&issues),
        "Found 1 issues:\n[{'title': 'It\\'s broken', 'number': 4}]"
    );
    let none = client
        .execute(GitLabOperation::GetIssues)
        .await
        .expect("no issues");
    assert_eq!(text(&none), "No open issues available");
    let issue = client
        .execute(GitLabOperation::GetIssue { issue_number: 4 })
        .await
        .expect("issue");
    assert_eq!(
        issue,
        json!({"title": "T", "body": null, "comments": [{"body": "hi", "user": "ann"}]})
    );
    let commits = client
        .execute(GitLabOperation::GetCommits {
            sha: Some("main"),
            path: None,
            since: Some("2025-01-01"),
            until: None,
            author: None,
        })
        .await
        .expect("commits");
    assert_eq!(
        commits,
        json!([{"sha": "c1", "author": "Ann", "createdAt": "2025-01-01T00:00:00Z", "message": "fix", "url": "https://gitlab.example.test/c1"}])
    );
    let urls = transport
        .requests
        .lock()
        .expect("requests")
        .iter()
        .map(|request| request.url.clone())
        .collect::<Vec<_>>();
    assert_eq!(
        urls,
        [
            format!("{PROJECT}/repository/branches?per_page=100&page=1"),
            format!("{PROJECT}/repository/branches?per_page=100&page=2"),
            format!("{PROJECT}/issues?state=opened"),
            format!("{PROJECT}/issues?state=opened"),
            format!("{PROJECT}/issues/4"),
            format!("{PROJECT}/issues/4/notes?page=1"),
            format!("{PROJECT}/repository/commits?ref_name=main&since=2025-01-01"),
        ]
    );
}

#[tokio::test]
async fn reads_slice_guard_and_move_the_active_branch() {
    let (client, transport) = client([
        file("one\ntwo\nthree\n"),
        file("needle here\nother\n"),
        file("a\n"),
        status(StatusCode::NOT_FOUND),
        file(&"x\n".repeat(150_000)),
        ok(json!([])),
    ]);
    let slice = client
        .execute(GitLabOperation::ReadFile {
            file_path: "src/a.py",
            branch: Some("feature"),
            start_line: Some(2),
            end_line: Some(3),
        })
        .await
        .expect("read");
    assert_eq!(text(&slice), "two\nthree\n");
    let grep = client
        .execute(GitLabOperation::GrepFile {
            file_path: "src/a.py",
            pattern: "NEEDLE",
            branch: None,
            is_regex: false,
            context_lines: 0,
        })
        .await
        .expect("grep");
    assert_eq!(
        text(&grep),
        "Found 1 match(es) for pattern 'NEEDLE' in src/a.py:\n\n\n--- Match 1 at line 1 ---\n> needle here"
    );
    let batch = client
        .execute(GitLabOperation::ReadMultipleFiles {
            file_paths: vec!["a.txt", "missing.txt", "big.txt"],
            branch: None,
            offset: None,
            limit: None,
        })
        .await
        .expect("batch");
    assert_eq!(batch["a.txt"], "a\n");
    assert_eq!(
        batch["missing.txt"],
        "Error reading file: the GitLab resource was not found"
    );
    assert_eq!(batch["big.txt"]["__result_status__"], "content_too_large");
    // The read set the active branch, which a listing then uses.
    client
        .execute(GitLabOperation::ListFolders {
            path: Some("src"),
            recursive: false,
            branch: None,
        })
        .await
        .expect("folders");
    let urls = transport
        .requests
        .lock()
        .expect("requests")
        .iter()
        .map(|request| request.url.clone())
        .collect::<Vec<_>>();
    assert_eq!(
        urls[0],
        format!("{PROJECT}/repository/files/src%2Fa.py?ref=feature")
    );
    assert_eq!(
        urls[1],
        format!("{PROJECT}/repository/files/src%2Fa.py?ref=feature")
    );
    assert_eq!(
        urls[5],
        format!(
            "{PROJECT}/repository/tree?recursive=false&path=src&ref=feature&per_page=100&page=1"
        )
    );
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ledger of every remote write's route and body.
async fn writes_keep_routes_bodies_messages_and_base_branch_protection() {
    let (client, transport) = client([
        // create_file: exists, then absent → create
        file("x"),
        status(StatusCode::NOT_FOUND),
        created(json!({"file_path": "new.md"})),
        // update_file
        file("alpha\nbeta\n"),
        created(json!({"id": "c2"})),
        // append_file
        file("alpha"),
        created(json!({"id": "c3"})),
        // delete_file
        status(StatusCode::NO_CONTENT),
        // create_pull_request
        created(json!({"iid": 12})),
        // comment_on_issue
        ok(json!({"iid": 3, "title": "t"})),
        created(json!({"id": 1})),
        // comment_on_pr
        ok(json!({"iid": 12, "title": "t"})),
        created(json!({"id": 2})),
    ]);
    let exists = client
        .execute(GitLabOperation::CreateFile {
            file_path: "a.md",
            contents: "x",
            branch: Some("feature"),
        })
        .await
        .expect("exists");
    assert_eq!(
        text(&exists),
        "File already exists at a.md. Use update_file instead"
    );
    let made = client
        .execute(GitLabOperation::CreateFile {
            file_path: "new.md",
            contents: "",
            branch: Some("feature"),
        })
        .await
        .expect("created");
    assert_eq!(text(&made), "Created file new.md");

    let protected = client
        .execute(GitLabOperation::UpdateFile {
            file_query: "a.md\nOLD <<<<\nx\n>>>> OLD\nNEW <<<<\ny\n>>>> NEW",
            branch: "main",
        })
        .await
        .expect("protected");
    assert_eq!(
        text(&protected),
        "You're attempting to commit directly to the main branch, which is protected. Please create a new branch and try again."
    );
    let no_markers = client
        .execute(GitLabOperation::UpdateFile {
            file_query: "a.md\nreplace everything",
            branch: "feature",
        })
        .await
        .expect("no markers");
    assert!(text(&no_markers).contains("No OLD/NEW marker pairs found"));
    let binary = client
        .execute(GitLabOperation::UpdateFile {
            file_query: "logo.png\nOLD <<<<\nx\n>>>> OLD\nNEW <<<<\ny\n>>>> NEW",
            branch: "feature",
        })
        .await
        .expect("binary");
    assert!(text(&binary).contains("Cannot edit binary/document file 'logo.png'"));
    let updated = client
        .execute(GitLabOperation::UpdateFile {
            file_query: "\n  src/a.py\nOLD <<<<\nbeta\n>>>> OLD\nNEW <<<<\ngamma\n>>>> NEW",
            branch: "feature",
        })
        .await
        .expect("updated");
    assert_eq!(text(&updated), "Updated file src/a.py");

    let empty = client
        .execute(GitLabOperation::AppendFile {
            file_path: "a.md",
            content: "",
            branch: "feature",
        })
        .await
        .expect("empty append");
    assert_eq!(
        text(&empty),
        "Content to be added is empty. Append file won't be completed"
    );
    let appended = client
        .execute(GitLabOperation::AppendFile {
            file_path: "a.md",
            content: "omega",
            branch: "feature",
        })
        .await
        .expect("append");
    assert_eq!(text(&appended), "Updated file a.md");
    let deleted = client
        .execute(GitLabOperation::DeleteFile {
            file_path: "a.md",
            branch: Some("feature"),
            commit_message: None,
        })
        .await
        .expect("delete");
    assert_eq!(text(&deleted), "Deleted file a.md");

    let same_branch = client
        .execute(GitLabOperation::CreatePullRequest {
            title: "t",
            body: "b",
            branch: "main",
        })
        .await
        .expect("refused");
    assert!(text(&same_branch).starts_with("Cannot make a pull request because"));
    let pr = client
        .execute(GitLabOperation::CreatePullRequest {
            title: "t",
            body: "b",
            branch: "feature",
        })
        .await
        .expect("pr");
    assert_eq!(text(&pr), "Successfully created PR number 12");
    let comment = client
        .execute(GitLabOperation::CommentOnIssue {
            comment_query: "3\n\nPlease add a test.",
        })
        .await
        .expect("issue comment");
    assert_eq!(text(&comment), "Commented on issue 3");
    let pr_comment = client
        .execute(GitLabOperation::CommentOnPr {
            pr_number: 12,
            comment: "LGTM",
        })
        .await
        .expect("pr comment");
    assert_eq!(text(&pr_comment), "Commented on merge request 12");

    let requests = transport.requests.lock().expect("requests").clone();
    let routes = requests
        .iter()
        .map(|request| {
            (
                request.method.to_string(),
                request.url.trim_start_matches(PROJECT).to_owned(),
            )
        })
        .collect::<Vec<_>>();
    let route = |method: &str, path: &str| (method.to_owned(), path.to_owned());
    assert_eq!(
        routes,
        [
            route("GET", "/repository/files/a.md?ref=feature"),
            route("GET", "/repository/files/new.md?ref=feature"),
            route("POST", "/repository/files/new.md"),
            route("GET", "/repository/files/src%2Fa.py?ref=feature"),
            route("POST", "/repository/commits"),
            route("GET", "/repository/files/a.md?ref=feature"),
            route("POST", "/repository/commits"),
            route(
                "DELETE",
                "/repository/files/a.md?branch=feature&commit_message=Delete+a.md"
            ),
            route("POST", "/merge_requests"),
            route("GET", "/issues/3"),
            route("POST", "/issues/3/notes"),
            route("GET", "/merge_requests/12"),
            route("POST", "/merge_requests/12/notes"),
        ]
    );
    assert_eq!(
        requests[2].body,
        Some(json!({"branch": "feature", "commit_message": "Create new.md", "content": ""}))
    );
    assert_eq!(
        requests[4].body,
        Some(
            json!({"branch": "feature", "commit_message": "Update src/a.py", "actions": [{
                "action": "update", "file_path": "src/a.py", "content": "alpha\ngamma\n",
                "last_commit_id": "abc123"
            }]})
        )
    );
    assert_eq!(
        requests[6].body.as_ref().expect("append body")["actions"][0]["content"],
        "alpha\nomega"
    );
    assert_eq!(
        requests[8].body,
        Some(
            json!({"source_branch": "feature", "target_branch": "main", "title": "t",
            "description": "b", "labels": ["created-by-agent"]})
        )
    );
    assert_eq!(
        requests[10].body,
        Some(json!({"body": "Please add a test."}))
    );
    assert!(
        requests
            .iter()
            .all(|request| request.effect == (request.method != Method::GET))
    );
}

#[tokio::test]
async fn merge_request_diffs_and_inline_comments_follow_the_sdk() {
    let mr = json!({
        "iid": 7, "title": "Fix", "description": null,
        "diff_refs": {"base_sha": "b", "head_sha": "h", "start_sha": "s"}
    });
    let changes = json!({"changes": [{
        "old_path": "a.py", "new_path": "a.py",
        "diff": "@@ -1,2 +1,2 @@\n-old\n+new\n same\n"
    }]});
    let (client, transport) = client([
        ok(mr.clone()),
        ok(changes.clone()),
        ok(mr.clone()),
        ok(changes.clone()),
        created(json!({"id": "d"})),
        ok(mr),
        ok(changes),
    ]);
    let diff = client
        .execute(GitLabOperation::GetPrChanges { pr_number: 7 })
        .await
        .expect("changes");
    assert_eq!(
        text(&diff),
        "title: Fix\ndescription: None\n\ndiff --git a/a.py b/a.py\n@@ -1,2 +1,2 @@\n-old\n+new\n same\n\n"
    );
    let comment = client
        .execute(GitLabOperation::CreatePrChangeComment {
            pr_number: 7,
            file_path: "a.py",
            line_number: 2,
            comment: "nice",
        })
        .await
        .expect("inline");
    assert_eq!(
        text(&comment),
        "Comment added successfully to line 2 in a.py on MR #7"
    );
    let missing = client
        .execute(GitLabOperation::CreatePrChangeComment {
            pr_number: 7,
            file_path: "b.py",
            line_number: 0,
            comment: "x",
        })
        .await
        .expect("missing change");
    assert!(text(&missing).starts_with("Failed to create comment on MR #7"));
    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(
        requests[4].url,
        format!("{PROJECT}/merge_requests/7/discussions")
    );
    assert_eq!(
        requests[4].body,
        Some(json!({"body": "nice", "position": {
            "new_line": 1, "new_path": "a.py", "base_sha": "b", "head_sha": "h",
            "start_sha": "s", "position_type": "text"
        }}))
    );
}

#[tokio::test]
async fn provider_failures_map_to_stable_gitlab_errors() {
    let (client, _) = client([
        status(StatusCode::UNAUTHORIZED),
        status(StatusCode::INTERNAL_SERVER_ERROR),
        GitLabOrgHttpResponse::non_json_fixture(StatusCode::OK),
    ]);
    let unauthorized = client
        .execute(GitLabOperation::GetIssue { issue_number: 1 })
        .await
        .expect_err("401");
    assert_eq!(unauthorized.code(), GitLabClientErrorCode::Authentication);
    let effect = client
        .execute(GitLabOperation::CommentOnPr {
            pr_number: 0,
            comment: "x",
        })
        .await
        .expect_err("zero iid");
    assert_eq!(effect.code(), GitLabClientErrorCode::InvalidInput);
    let unknown = client
        .execute(GitLabOperation::CreatePullRequest {
            title: "t",
            body: "b",
            branch: "feature",
        })
        .await
        .expect_err("500 on an effect");
    assert_eq!(unknown.code(), GitLabClientErrorCode::UnknownOutcome);
    let adk = unknown.into_adk();
    assert_eq!(adk.code, "gitlab.effect.unknown_outcome");
    let shape = client
        .execute(GitLabOperation::GetIssues)
        .await
        .expect_err("non-json");
    assert_eq!(shape.code(), GitLabClientErrorCode::InvalidResponse);
    assert_eq!(
        GitLabClientError::fixture(GitLabClientErrorCode::NotFound).to_string(),
        "the GitLab resource was not found"
    );
}

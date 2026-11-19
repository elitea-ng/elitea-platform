//! Contract tests for the Bitbucket family (SDK `bitbucket`), Server and Cloud.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use elitea_connectors::transport::{Body, Method, Request, StatusCode};
use reqwest::header::{AUTHORIZATION, CONTENT_TYPE};
use serde_json::{Map, Value, json};

use super::families::bitbucket::client::{
    BitbucketApi, BitbucketClient, BitbucketClientError, BitbucketClientErrorCode,
    BitbucketHttpResponse, BitbucketOperation, BitbucketTarget, BitbucketTransport,
};
use super::families::bitbucket::config::{
    BitbucketConfigErrorCode, BitbucketHosting, BitbucketToolkitConfig,
};
use super::families::bitbucket::tools::{
    BitbucketToolsetErrorCode, test_build_with_api, test_catalog,
};
use super::families::connector_client::IntoAdk;
use super::policy::ToolAdmissionPolicy;

const SERVER_REPO: &str = "https://bitbucket.example.test/ctx/rest/api/1.0/projects/PROJ/repos/app";
const CLOUD_REPO: &str = "https://api.bitbucket.org/2.0/repositories/team/app";

fn settings(url: &str, cloud: &Value) -> Map<String, Value> {
    json!({
        "bitbucket_configuration": {
            "url": url,
            "username": "bot",
            "password": "app-password-value"
        },
        "project": "PROJ",
        "repository": "app",
        "branch": "main",
        "cloud": cloud,
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("Bitbucket fixture is an object")
}

fn server_config() -> BitbucketToolkitConfig {
    BitbucketToolkitConfig::parse(&settings(
        "https://bitbucket.example.test/ctx/",
        &Value::Null,
    ))
    .expect("server fixture")
}

fn cloud_config() -> BitbucketToolkitConfig {
    let mut cloud = settings("https://bitbucket.org", &Value::Null);
    cloud.insert("project".to_owned(), json!("team"));
    BitbucketToolkitConfig::parse(&cloud).expect("cloud fixture")
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
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Bitbucket policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("bitbucket-test").with_function_call_id("bitbucket-call"))
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
            ("create_pull_request", "write"),
            ("create_file", "write"),
            ("delete_file", "delete"),
            ("read_file", "read"),
            ("update_file", "write"),
            ("set_active_branch", "write"),
            ("get_pull_requests_commits", "read"),
            ("get_pull_request", "read"),
            ("get_pull_requests_changes", "read"),
            ("add_pull_request_comment", "write"),
            ("close_pull_request", "write"),
            ("read_multiple_files", "read"),
            ("grep_file", "read"),
        ]
    );
}

#[test]
fn hosting_is_explicit_or_follows_the_bitbucket_org_host() {
    assert_eq!(server_config().hosting(), BitbucketHosting::Server);
    assert_eq!(cloud_config().hosting(), BitbucketHosting::Cloud);
    let explicit =
        BitbucketToolkitConfig::parse(&settings("https://api.bitbucket.org", &Value::Bool(false)))
            .expect("explicit server");
    assert_eq!(explicit.hosting(), BitbucketHosting::Server);
    // Host matching, not substring matching.
    let spoof = BitbucketToolkitConfig::parse(&settings(
        "https://bitbucket.org.attacker.test",
        &Value::Null,
    ))
    .expect("spoof host parses");
    assert_eq!(spoof.hosting(), BitbucketHosting::Server);

    let Err(error) =
        BitbucketToolkitConfig::parse(&settings("http://bitbucket.example.test", &Value::Null))
    else {
        panic!("plain HTTP is refused");
    };
    assert_eq!(error.code(), BitbucketConfigErrorCode::InvalidConfiguration);
    assert!(!format!("{error:?}").contains("app-password-value"));
    let mut slash = settings("https://bitbucket.example.test", &Value::Null);
    slash.insert("repository".to_owned(), json!("a/b"));
    assert!(BitbucketToolkitConfig::parse(&slash).is_err());
}

#[derive(Default)]
struct FixtureApi {
    calls: Mutex<Vec<String>>,
}

#[async_trait]
impl BitbucketApi for FixtureApi {
    async fn execute(
        &self,
        operation: BitbucketOperation<'_>,
    ) -> Result<Value, BitbucketClientError> {
        let name = match operation {
            BitbucketOperation::ListBranches { limit, .. } => format!("list_branches:{limit}"),
            BitbucketOperation::GetPullRequest { pr_id } => format!("get_pull_request:{pr_id}"),
            BitbucketOperation::AddPullRequestComment { inline, .. } => {
                format!("comment:{}", inline.is_some())
            }
            _ => "other".to_owned(),
        };
        self.calls.lock().expect("calls").push(name.clone());
        Ok(json!({"call": name}))
    }
}

async fn tools_for(
    api: &Arc<FixtureApi>,
    selected: &[&str],
) -> Result<Vec<Arc<dyn adk_core::Tool>>, BitbucketToolsetErrorCode> {
    let api_trait: Arc<dyn BitbucketApi> = api.clone();
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(*name))
        .collect::<Vec<_>>();
    let toolset = test_build_with_api("repo", &selected, &policy(&[]), &api_trait)
        .map_err(|error| error.code())?;
    let readonly: Arc<dyn ReadonlyContext> = context();
    Ok(toolset.tools(readonly).await.expect("Bitbucket tools"))
}

#[tokio::test]
async fn schemas_selection_and_arguments_follow_the_sdk() {
    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &[]).await.expect("complete toolset");
    assert_eq!(tools.len(), 17);
    for (tool, (name, group)) in tools.iter().zip(test_catalog()) {
        assert_eq!(tool.name(), name);
        assert_eq!(tool.is_read_only(), group == "read");
        assert!(tool.description().starts_with("Toolkit: repo\n"));
        let schema = tool.parameters_schema().expect("schema");
        assert_eq!(schema["additionalProperties"], false);
        for forbidden in ["bitbucket.example.test", "app-password-value", "PROJ"] {
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
    by_name("list_branches_in_repo")
        .execute(context(), json!({"branch_wildcard": null}))
        .await
        .expect("default limit");
    by_name("get_pull_request")
        .execute(context(), json!({"pr_id": " 42 "}))
        .await
        .expect("string id");
    by_name("add_pull_request_comment")
        .execute(
            context(),
            json!({"pr_id": "4", "content": "x", "inline": {"to": 3, "path": "a.py"}}),
        )
        .await
        .expect("inline comment");
    assert_eq!(
        *api.calls.lock().expect("calls"),
        ["list_branches:20", "get_pull_request:42", "comment:true"]
    );
    for (name, arguments) in [
        ("get_pull_request", json!({"pr_id": "abc"})),
        ("get_pull_request", json!({"pr_id": "0"})),
        (
            "add_pull_request_comment",
            json!({"pr_id": "1", "content": "x", "inline": "no"}),
        ),
        ("create_branch", json!({"branch_name": "x", "extra": 1})),
    ] {
        assert!(by_name(name).execute(context(), arguments).await.is_err());
    }
    let subset = tools_for(&api, &["read_file", "search_index"])
        .await
        .expect("index tool dropped");
    assert_eq!(subset.len(), 1);
    assert_eq!(
        tools_for(&api, &["index_data"]).await.err(),
        Some(BitbucketToolsetErrorCode::UnsupportedSelection)
    );
    let api_trait: Arc<dyn BitbucketApi> = api.clone();
    let blocked = test_build_with_api(
        "repo",
        &[],
        &policy(&[("bitbucket", &["close_pull_request"])]),
        &api_trait,
    )
    .expect("policy filtered");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(blocked.tools(readonly).await.expect("tools").len(), 16);
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &[]).await.expect("complete toolset");
    super::sdk_conformance::assert_sdk_conformance("bitbucket", &tools);
}

#[derive(Clone, Debug)]
struct Captured {
    method: Method,
    url: String,
    body: String,
    content_type: Option<String>,
    authorization_sensitive: bool,
    effect: bool,
}

struct FixtureTransport {
    requests: Mutex<Vec<Captured>>,
    responses: Mutex<VecDeque<BitbucketHttpResponse>>,
}

#[async_trait]
impl BitbucketTransport for FixtureTransport {
    async fn execute(
        &self,
        request: Request,
        effect: bool,
    ) -> Result<BitbucketHttpResponse, BitbucketClientError> {
        let authorization = request.headers().get(AUTHORIZATION);
        assert_eq!(
            authorization.and_then(|value| value.to_str().ok()),
            Some("Basic Ym90OmFwcC1wYXNzd29yZC12YWx1ZQ==")
        );
        self.requests.lock().expect("requests").push(Captured {
            method: request.method().clone(),
            url: request.url().to_string(),
            body: request
                .body()
                .and_then(Body::as_bytes)
                .map(|bytes| String::from_utf8_lossy(bytes).into_owned())
                .unwrap_or_default(),
            content_type: request
                .headers()
                .get(CONTENT_TYPE)
                .and_then(|value| value.to_str().ok())
                .map(ToOwned::to_owned),
            authorization_sensitive: authorization
                .is_some_and(reqwest::header::HeaderValue::is_sensitive),
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

fn client(
    config: BitbucketToolkitConfig,
    responses: impl IntoIterator<Item = BitbucketHttpResponse>,
) -> (BitbucketClient, Arc<FixtureTransport>) {
    let transport = Arc::new(FixtureTransport {
        requests: Mutex::new(Vec::new()),
        responses: Mutex::new(responses.into_iter().collect()),
    });
    let transport_trait: Arc<dyn BitbucketTransport> = transport.clone();
    (BitbucketClient::fixture(config, transport_trait), transport)
}

fn ok(body: Value) -> BitbucketHttpResponse {
    BitbucketHttpResponse::json(StatusCode::OK, body)
}

fn text(value: &Value) -> &str {
    value.as_str().expect("a text result")
}

fn routes(transport: &FixtureTransport, prefix: &str) -> Vec<String> {
    transport
        .requests
        .lock()
        .expect("requests")
        .iter()
        .map(|request| {
            format!(
                "{} {}",
                request.method,
                request.url.trim_start_matches(prefix)
            )
        })
        .collect()
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One Server ledger over shared active-branch state.
async fn server_routes_messages_and_branch_rules() {
    let branches_page_one = json!({"values": [{"displayId": "main"}, {"displayId": "feature/a"}],
        "isLastPage": false, "nextPageStart": 2});
    let branches_page_two = json!({"values": [{"displayId": "release/1"}], "isLastPage": true});
    let (client, transport) = client(
        server_config(),
        [
            ok(branches_page_one.clone()),
            ok(branches_page_two.clone()),
            ok(json!({"id": "refs/heads/feature/b"})),
            BitbucketHttpResponse::json(
                StatusCode::CONFLICT,
                json!({"errors": [{"message": "Branch 'feature/a' already exists"}]}),
            ),
            ok(branches_page_one.clone()),
            ok(branches_page_two.clone()),
            ok(branches_page_one),
            ok(branches_page_two),
            BitbucketHttpResponse::text(StatusCode::NO_CONTENT, ""),
            ok(json!({"values": ["a.py", "pkg/b.py"], "isLastPage": true})),
        ],
    );
    let listed = client
        .execute(BitbucketOperation::ListBranches {
            limit: 2,
            branch_wildcard: Some("*a*"),
        })
        .await
        .expect("branches");
    // fnmatch filter first, then the limit.
    assert_eq!(text(&listed), "Found branches: main, feature/a");
    let created = client
        .execute(BitbucketOperation::CreateBranch {
            branch_name: "feature/b",
        })
        .await
        .expect("create");
    assert_eq!(
        text(&created),
        "Branch feature/b created successfully and set as active"
    );
    let exists = client
        .execute(BitbucketOperation::CreateBranch {
            branch_name: "feature/a",
        })
        .await
        .expect("exists");
    assert_eq!(
        text(&exists),
        "Branch feature/a already exists. set it as active"
    );
    let refused = client
        .execute(BitbucketOperation::DeleteBranch {
            branch_name: "Master",
        })
        .await
        .expect("protected");
    assert!(text(&refused).contains("forbidden for safety reasons"));
    let active = client
        .execute(BitbucketOperation::DeleteBranch {
            branch_name: "feature/a",
        })
        .await
        .expect("active");
    assert!(text(&active).contains("currently the active branch"));
    let deleted = client
        .execute(BitbucketOperation::DeleteBranch {
            branch_name: "release/1",
        })
        .await
        .expect("deleted");
    assert_eq!(
        text(&deleted),
        "Branch 'release/1' has been deleted successfully."
    );
    let files = client
        .execute(BitbucketOperation::ListFiles {
            path: Some("src"),
            recursive: false,
            branch: None,
        })
        .await
        .expect("files");
    assert_eq!(files, json!(["src/a.py"]));

    let requests = transport.requests.lock().expect("requests").clone();
    assert!(
        requests
            .iter()
            .all(|request| request.authorization_sensitive)
    );
    assert_eq!(
        routes(&transport, SERVER_REPO),
        [
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=0",
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=2",
            "POST /branches",
            "POST /branches",
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=0",
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=2",
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=0",
            "GET /branches?orderBy=MODIFICATION&details=true&boostMatches=false&limit=100&start=2",
            "DELETE https://bitbucket.example.test/ctx/rest/branch-utils/1.0/projects/PROJ/repos/app/branches",
            "GET /files/src?at=feature%2Fa&limit=100&start=0",
        ]
    );
    assert_eq!(
        serde_json::from_str::<Value>(&requests[2].body).expect("json"),
        json!({"name": "feature/b", "startPoint": "main", "message": ""})
    );
    // The second branch is cut from the first, which became active.
    assert_eq!(
        serde_json::from_str::<Value>(&requests[3].body).expect("json")["startPoint"],
        "feature/b"
    );
    assert_eq!(requests[8].body, "{\"name\":\"release/1\"}");
    assert!(requests[8].effect);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One Server file/PR ledger.
async fn server_files_and_pull_requests_keep_sdk_shapes() {
    let (client, transport) = client(
        server_config(),
        [
            // create_file: exists, then absent → upload
            BitbucketHttpResponse::text(StatusCode::OK, "x"),
            BitbucketHttpResponse::json(StatusCode::NOT_FOUND, json!({"errors": []})),
            ok(json!({"id": "c1"})),
            // update_file: read, head, upload
            BitbucketHttpResponse::text(StatusCode::OK, "alpha\nbeta\n"),
            ok(json!({"values": [{"id": "head123"}]})),
            ok(json!({"id": "c2"})),
            // create_pull_request
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({"id": 9, "open": true})),
            // close_pull_request: read version, decline, comment
            ok(json!({"id": 9, "version": 3, "state": "OPEN"})),
            ok(json!({"id": 9, "state": "DECLINED"})),
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({"id": 1, "text": "bye"})),
            // get_pull_requests_changes (paged)
            ok(
                json!({"values": [{"path": {"toString": "a.py"}, "type": "MODIFY"}], "isLastPage": true}),
            ),
        ],
    );
    let exists = client
        .execute(BitbucketOperation::CreateFile {
            file_path: "a.md",
            contents: "x",
            branch: Some("feature"),
        })
        .await
        .expect("exists");
    assert_eq!(
        text(&exists),
        "File already exists: a.md. Use update_file() to modify existing files."
    );
    let created = client
        .execute(BitbucketOperation::CreateFile {
            file_path: "docs/new.md",
            contents: "hello",
            branch: Some("feature"),
        })
        .await
        .expect("created");
    assert_eq!(text(&created), "File has been created: docs/new.md.");
    let updated = client
        .execute(BitbucketOperation::UpdateFile {
            file_path: "src/a.py",
            update_query: "OLD <<<<\nbeta\n>>>> OLD\nNEW <<<<\ngamma\n>>>> NEW",
            branch: Some("feature"),
        })
        .await
        .expect("updated");
    assert_eq!(text(&updated), "Update src/a.py");
    let no_markers = client
        .execute(BitbucketOperation::UpdateFile {
            file_path: "src/a.py",
            update_query: "just text",
            branch: Some("feature"),
        })
        .await
        .expect("no markers");
    assert!(text(&no_markers).starts_with("No OLD/NEW marker pairs found"));
    let invalid_json = client
        .execute(BitbucketOperation::CreatePullRequest {
            pr_json_data: "{not json",
        })
        .await
        .expect("guidance");
    assert!(text(&invalid_json).starts_with("Make sure your pr_json matches"));
    let pr = client
        .execute(BitbucketOperation::CreatePullRequest {
            pr_json_data: r#"{"title":"T","fromRef":{"id":"refs/heads/feature"},"toRef":{"id":"refs/heads/main"}}"#,
        })
        .await
        .expect("pr");
    assert_eq!(
        text(&pr),
        "Successfully created PR\n{'id': 9, 'open': True}"
    );
    let closed = client
        .execute(BitbucketOperation::ClosePullRequest {
            pr_id: 9,
            message: Some("bye"),
        })
        .await
        .expect("closed");
    assert_eq!(
        text(&closed),
        "Successfully closed pull request 9\n{'id': 9, 'state': 'DECLINED'}"
    );
    let inline = client
        .execute(BitbucketOperation::AddPullRequestComment {
            pr_id: 9,
            content: "x",
            inline: Some(&Map::from_iter([("path".to_owned(), json!("a.py"))])),
        })
        .await
        .expect("inline refused on server");
    assert!(text(&inline).contains("not supported on Bitbucket Server"));
    let changes = client
        .execute(BitbucketOperation::GetPullRequestChanges { pr_id: 9 })
        .await
        .expect("changes");
    assert_eq!(changes[0]["type"], "MODIFY");

    assert_eq!(
        routes(&transport, SERVER_REPO),
        [
            "GET /raw/a.md?at=feature",
            "GET /raw/docs/new.md?at=feature",
            "PUT /browse/docs/new.md",
            "GET /raw/src/a.py?at=feature",
            "GET /commits?merges=include&until=feature&limit=1",
            "PUT /browse/src/a.py",
            "POST /pull-requests",
            "GET /pull-requests/9",
            "POST /pull-requests/9/decline?version=3",
            "POST /pull-requests/9/comments",
            "GET /pull-requests/9/changes?limit=100&start=0",
        ]
    );
    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(
        requests[2].content_type.as_deref(),
        Some("multipart/form-data; boundary=elitea-bitbucket-form-boundary")
    );
    assert!(
        requests[2]
            .body
            .contains("name=\"content\"\r\n\r\nhello\r\n")
    );
    assert!(
        requests[2]
            .body
            .contains("name=\"message\"\r\n\r\nCreate docs/new.md\r\n")
    );
    assert!(
        requests[5]
            .body
            .contains("name=\"content\"\r\n\r\nalpha\ngamma\n\r\n")
    );
    assert!(
        requests[5]
            .body
            .contains("name=\"sourceCommitId\"\r\n\r\nhead123\r\n")
    );
    assert_eq!(requests[9].body, "{\"text\":\"bye\"}");
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One Cloud ledger.
async fn cloud_routes_follow_hashes_links_and_redirects() {
    let main = || ok(json!({"name": "main", "target": {"hash": "abcdef1234"}}));
    let (client, transport) = client(
        cloud_config(),
        [
            // read_file: branch hash, raw source
            main(),
            BitbucketHttpResponse::text(StatusCode::OK, "one\ntwo\n"),
            // list_files: branch hash, two pages linked by `next`
            main(),
            ok(json!({"values": [{"path": "a.py"}],
                "next": format!("{CLOUD_REPO}/src/abcdef1234/?page=2")})),
            ok(json!({"values": [{"path": "pkg/b.py"}]})),
            // create_branch: hash, create
            main(),
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({"name": "feature"})),
            // create_file on the new active branch: hash, 404, post
            ok(json!({"name": "feature", "target": {"hash": "fedcba4321"}})),
            BitbucketHttpResponse::json(StatusCode::NOT_FOUND, json!({"type": "error"})),
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({})),
            // create_pull_request
            BitbucketHttpResponse::json(
                StatusCode::CREATED,
                json!({"links": {"self": {"href": "https://api.bitbucket.org/pr/5"}}}),
            ),
            // get_pull_requests_changes: redirect then diff
            BitbucketHttpResponse::redirect(&format!(
                "{CLOUD_REPO}/diff/team/app:abc%0Ddef?from_pullrequest_id=5"
            )),
            BitbucketHttpResponse::text(StatusCode::OK, "diff --git a/a b/a\n"),
            // add_pull_request_comment
            BitbucketHttpResponse::json(
                StatusCode::CREATED,
                json!({"links": {"html": {"href": "https://bitbucket.org/c/1"}}}),
            ),
            // close_pull_request: not open
            ok(json!({"id": 5, "state": "MERGED"})),
        ],
    );
    let read = client
        .execute(BitbucketOperation::ReadFile {
            file_path: "src/a.py",
            branch: Some("  "),
            start_line: Some(2),
            end_line: None,
        })
        .await
        .expect("read");
    assert_eq!(text(&read), "two\n");
    let files = client
        .execute(BitbucketOperation::ListFiles {
            path: None,
            recursive: true,
            branch: None,
        })
        .await
        .expect("files");
    assert_eq!(files, json!(["a.py", "pkg/b.py"]));
    client
        .execute(BitbucketOperation::CreateBranch {
            branch_name: "feature",
        })
        .await
        .expect("branch");
    let created = client
        .execute(BitbucketOperation::CreateFile {
            file_path: "new.md",
            contents: "hi there",
            branch: None,
        })
        .await
        .expect("create");
    assert_eq!(text(&created), "File has been created: new.md.");
    let pr = client
        .execute(BitbucketOperation::CreatePullRequest {
            pr_json_data: r#"{"title":"T","source":{"branch":{"name":"feature"}}}"#,
        })
        .await
        .expect("pr");
    assert_eq!(
        text(&pr),
        "Successfully created PR\nhttps://api.bitbucket.org/pr/5"
    );
    let diff = client
        .execute(BitbucketOperation::GetPullRequestChanges { pr_id: 5 })
        .await
        .expect("diff");
    assert_eq!(diff, json!({"raw_response": "diff --git a/a b/a\n"}));
    let comment = client
        .execute(BitbucketOperation::AddPullRequestComment {
            pr_id: 5,
            content: "nit",
            inline: Some(&Map::from_iter([
                ("to".to_owned(), json!(3)),
                ("path".to_owned(), json!("a.py")),
            ])),
        })
        .await
        .expect("comment");
    assert_eq!(text(&comment), "https://bitbucket.org/c/1");
    let merged = client
        .execute(BitbucketOperation::ClosePullRequest {
            pr_id: 5,
            message: None,
        })
        .await
        .expect("not open");
    assert!(text(&merged).ends_with("Pull Request isn't open"));

    assert_eq!(
        routes(&transport, CLOUD_REPO),
        [
            "GET /refs/branches/main",
            "GET /src/abcdef1234/src/a.py",
            "GET /refs/branches/main",
            "GET /src/abcdef1234/?max_depth=100&fields=values.path%2Cnext&q=type%3D%22commit_file%22&pagelen=100",
            "GET /src/abcdef1234/?page=2",
            "GET /refs/branches/main",
            "POST /refs/branches",
            "GET /refs/branches/feature",
            "GET /src/fedcba4321/new.md",
            "POST /src",
            "POST /pullrequests",
            "GET /pullrequests/5/diff",
            "GET /diff/team/app:abc%0Ddef?from_pullrequest_id=5",
            "POST /pullrequests/5/comments",
            "GET /pullrequests/5",
        ]
    );
    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(
        serde_json::from_str::<Value>(&requests[6].body).expect("json"),
        json!({"name": "feature", "target": {"hash": "abcdef1234"}})
    );
    assert_eq!(
        requests[9].content_type.as_deref(),
        Some("application/x-www-form-urlencoded")
    );
    assert_eq!(
        requests[9].body,
        "branch=feature&message=Create+new.md&new.md=hi+there"
    );
    assert_eq!(
        serde_json::from_str::<Value>(&requests[13].body).expect("json"),
        json!({"content": {"raw": "nit"}, "inline": {"path": "a.py", "to": 3}})
    );
}

#[tokio::test]
async fn foreign_links_and_provider_failures_fail_closed() {
    let (client, _) = client(
        cloud_config(),
        [
            ok(json!({"name": "main", "target": {"hash": "abcdef1234"}})),
            ok(json!({"values": [], "next": "https://evil.test/2.0/repositories/team/app/src"})),
            ok(
                json!({"values": [{"name": "main"}], "next": "https://api.bitbucket.org/2.0/repositories/other/repo/refs/branches?page=2"}),
            ),
            BitbucketHttpResponse::json(StatusCode::UNAUTHORIZED, json!({})),
            BitbucketHttpResponse::json(StatusCode::INTERNAL_SERVER_ERROR, json!({})),
        ],
    );
    let foreign = client
        .execute(BitbucketOperation::ListFiles {
            path: None,
            recursive: true,
            branch: None,
        })
        .await
        .expect_err("foreign origin");
    assert_eq!(foreign.code(), BitbucketClientErrorCode::InvalidResponse);
    let other_repo = client
        .execute(BitbucketOperation::ListBranches {
            limit: 20,
            branch_wildcard: None,
        })
        .await
        .expect_err("other repository");
    assert_eq!(other_repo.code(), BitbucketClientErrorCode::InvalidResponse);
    let unauthorized = client
        .execute(BitbucketOperation::GetPullRequest { pr_id: 1 })
        .await
        .expect_err("401");
    assert_eq!(
        unauthorized.code(),
        BitbucketClientErrorCode::Authentication
    );
    let unknown = client
        .execute(BitbucketOperation::CreatePullRequest {
            pr_json_data: "{\"title\":\"T\"}",
        })
        .await
        .expect_err("500 on an effect");
    assert_eq!(unknown.code(), BitbucketClientErrorCode::UnknownOutcome);
    assert_eq!(unknown.into_adk().code, "bitbucket.effect.unknown_outcome");
    assert_eq!(
        BitbucketClientError::fixture(BitbucketClientErrorCode::NotFound).to_string(),
        "the Bitbucket resource was not found"
    );
}

// -- delete_file (upstream SDK, added after the 2026-08-07 pin; S10 #2265) ---

#[tokio::test]
async fn delete_file_is_refused_on_server_with_the_sdk_explanation_and_checks_the_file_first() {
    let (client, transport) = client(
        server_config(),
        [
            // present: the read succeeds, then the unsupported answer
            BitbucketHttpResponse::text(StatusCode::OK, "x"),
            // absent
            BitbucketHttpResponse::json(StatusCode::NOT_FOUND, json!({"errors": []})),
        ],
    );
    let refused = client
        .execute(BitbucketOperation::DeleteFile {
            file_path: "a.md",
            branch: Some("feature"),
            commit_message: None,
        })
        .await
        .expect("explained");
    assert!(
        text(&refused)
            .starts_with("Deleting a file is not supported by the Bitbucket Server REST API."),
        "{refused}"
    );
    let missing = client
        .execute(BitbucketOperation::DeleteFile {
            file_path: "gone.md",
            branch: Some("feature"),
            commit_message: Some("drop"),
        })
        .await
        .expect("missing");
    assert_eq!(
        text(&missing),
        "File 'gone.md' not found on branch 'feature'"
    );
    // Nothing was written: only the two reads went out.
    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(requests.len(), 2);
    assert!(requests.iter().all(|request| request.method == Method::GET));
    assert!(requests.iter().all(|request| !request.effect));
}

#[tokio::test]
async fn delete_file_posts_a_files_field_to_cloud_src_with_the_default_message() {
    let main = || ok(json!({"name": "main", "target": {"hash": "abcdef1234"}}));
    let (client, transport) = client(
        cloud_config(),
        [
            // default message: branch hash, read, delete
            main(),
            BitbucketHttpResponse::text(StatusCode::OK, "x"),
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({})),
            // explicit message and branch
            main(),
            BitbucketHttpResponse::text(StatusCode::OK, "x"),
            BitbucketHttpResponse::json(StatusCode::CREATED, json!({})),
        ],
    );
    let deleted = client
        .execute(BitbucketOperation::DeleteFile {
            file_path: "docs/old.md",
            branch: None,
            commit_message: None,
        })
        .await
        .expect("deleted");
    assert_eq!(text(&deleted), "File has been deleted: docs/old.md.");
    client
        .execute(BitbucketOperation::DeleteFile {
            file_path: "docs/old.md",
            branch: Some("main"),
            commit_message: Some("drop the page"),
        })
        .await
        .expect("deleted with message");
    assert_eq!(
        routes(&transport, CLOUD_REPO),
        [
            "GET /refs/branches/main",
            "GET /src/abcdef1234/docs/old.md",
            "POST /src",
            "GET /refs/branches/main",
            "GET /src/abcdef1234/docs/old.md",
            "POST /src",
        ]
    );
    let requests = transport.requests.lock().expect("requests").clone();
    assert_eq!(
        requests[2].content_type.as_deref(),
        Some("application/x-www-form-urlencoded")
    );
    assert_eq!(
        requests[2].body,
        "branch=main&files=docs%2Fold.md&message=Delete+docs%2Fold.md"
    );
    assert_eq!(
        requests[5].body,
        "branch=main&files=docs%2Fold.md&message=drop+the+page"
    );
    assert!(requests[2].effect && requests[2].authorization_sensitive);
    assert!(!requests[1].effect);
}

#[tokio::test]
async fn delete_file_maps_provider_failures_and_rejects_bad_arguments() {
    for (status, code) in [
        (
            StatusCode::UNAUTHORIZED,
            BitbucketClientErrorCode::Authentication,
        ),
        (
            StatusCode::FORBIDDEN,
            BitbucketClientErrorCode::Authorization,
        ),
        (
            StatusCode::TOO_MANY_REQUESTS,
            BitbucketClientErrorCode::RateLimited,
        ),
    ] {
        let (client, _) = client(
            server_config(),
            [BitbucketHttpResponse::json(status, json!({"errors": []}))],
        );
        let error = client
            .execute(BitbucketOperation::DeleteFile {
                file_path: "a.md",
                branch: Some("feature"),
                commit_message: None,
            })
            .await
            .expect_err("provider failure");
        assert_eq!(error.code(), code);
    }
    // A failing delete request on Cloud is an error, not a success message.
    let (client, _) = client(
        cloud_config(),
        [
            ok(json!({"name": "main", "target": {"hash": "abcdef1234"}})),
            BitbucketHttpResponse::text(StatusCode::OK, "x"),
            BitbucketHttpResponse::json(StatusCode::FORBIDDEN, json!({})),
        ],
    );
    let denied = client
        .execute(BitbucketOperation::DeleteFile {
            file_path: "a.md",
            branch: None,
            commit_message: None,
        })
        .await
        .expect_err("denied");
    assert_eq!(denied.code(), BitbucketClientErrorCode::Authorization);

    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &["delete_file"]).await.expect("tool");
    assert_eq!(tools.len(), 1);
    assert!(!tools[0].is_read_only());
    let schema = tools[0].parameters_schema().expect("schema");
    assert_eq!(schema["required"], json!(["file_path", "branch"]));
    for arguments in [
        json!({"branch": "main"}),
        json!({"file_path": "  ", "branch": "main"}),
        json!({"file_path": "a.md", "branch": "main", "extra": 1}),
        json!({"file_path": 4, "branch": "main"}),
    ] {
        assert!(tools[0].execute(context(), arguments).await.is_err());
    }
    tools[0]
        .execute(context(), json!({"file_path": "a.md", "branch": "main"}))
        .await
        .expect("valid call");
}

// -- per-call repository (upstream SDK `project` / `repository`; S10 #2265) ---

fn target(project: Option<&str>, repository: Option<&str>) -> BitbucketTarget {
    BitbucketTarget {
        project: project.map(ToOwned::to_owned),
        repository: repository.map(ToOwned::to_owned),
    }
}

#[tokio::test]
async fn every_tool_takes_the_repository_it_acts_on() {
    let api = Arc::new(FixtureApi::default());
    let tools = tools_for(&api, &[]).await.expect("complete toolset");
    for tool in &tools {
        let schema = tool.parameters_schema().expect("schema");
        for argument in ["project", "repository"] {
            assert_eq!(
                schema["properties"][argument]["type"],
                json!(["string", "null"]),
                "{} {argument}",
                tool.name()
            );
            assert!(
                !schema["required"]
                    .as_array()
                    .expect("required")
                    .contains(&json!(argument))
            );
        }
    }
    // The arguments reach the call and a malformed one is refused.
    let by_name = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .cloned()
            .expect("tool")
    };
    by_name("get_pull_request")
        .execute(
            context(),
            json!({"pr_id": "3", "project": "OTHER", "repository": null}),
        )
        .await
        .expect("targeted call");
    for arguments in [
        json!({"pr_id": "3", "project": 4}),
        json!({"pr_id": "3", "repository": ["a"]}),
    ] {
        assert!(
            by_name("get_pull_request")
                .execute(context(), arguments)
                .await
                .is_err()
        );
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ledger over the foreign-client cache.
async fn a_call_can_name_another_repository_and_it_keeps_its_own_active_branch() {
    let (client, transport) = client(
        server_config(),
        [
            // OTHER/lib: default branch, then the raw read on it
            ok(json!({"id": "refs/heads/trunk", "displayId": "trunk"})),
            BitbucketHttpResponse::text(StatusCode::OK, "lib text"),
            // OTHER/lib again (cached): branch list for set_active_branch
            ok(json!({"values": [{"displayId": "dev"}], "isLastPage": true})),
            // OTHER/lib reads on the branch it switched to
            BitbucketHttpResponse::text(StatusCode::OK, "dev text"),
            // the configured repository still reads on its own `main`
            BitbucketHttpResponse::text(StatusCode::OK, "app text"),
            // a half-named target takes the other half from the toolkit
            ok(json!({"id": "refs/heads/main", "displayId": "main"})),
            BitbucketHttpResponse::text(StatusCode::OK, "half text"),
        ],
    );
    let read = |path: &'static str| BitbucketOperation::ReadFile {
        file_path: path,
        branch: None,
        start_line: None,
        end_line: None,
    };
    let other = target(Some("OTHER"), Some("lib"));
    let first = client
        .execute_for(&other, read("a.md"))
        .await
        .expect("foreign read");
    assert_eq!(text(&first), "lib text");
    let switched = client
        .execute_for(
            &target(Some("other"), Some("LIB")),
            BitbucketOperation::SetActiveBranch { branch_name: "dev" },
        )
        .await
        .expect("switched");
    assert_eq!(text(&switched), "Switched to branch `dev` in `OTHER/lib`");
    let on_dev = client
        .execute_for(&other, read("a.md"))
        .await
        .expect("foreign read on dev");
    assert_eq!(text(&on_dev), "dev text");
    // Placeholders pipelines send for "nothing" mean the toolkit's own repository.
    let own = client
        .execute_for(&target(Some("null"), Some(" 'None' ")), read("a.md"))
        .await
        .expect("own repository");
    assert_eq!(text(&own), "app text");
    let half = client
        .execute_for(&target(None, Some("lib")), read("b.md"))
        .await
        .expect("half-named");
    assert_eq!(text(&half), "half text");

    let requests = transport.requests.lock().expect("requests").clone();
    let urls: Vec<&str> = requests
        .iter()
        .map(|request| request.url.as_str())
        .collect();
    let base = "https://bitbucket.example.test/ctx/rest/api/1.0/projects";
    assert_eq!(urls.len(), 7);
    assert_eq!(urls[0], format!("{base}/OTHER/repos/lib/default-branch"));
    assert_eq!(urls[1], format!("{base}/OTHER/repos/lib/raw/a.md?at=trunk"));
    assert!(urls[2].starts_with(&format!("{base}/OTHER/repos/lib/branches?")));
    assert_eq!(urls[3], format!("{base}/OTHER/repos/lib/raw/a.md?at=dev"));
    assert_eq!(urls[4], format!("{base}/PROJ/repos/app/raw/a.md?at=main"));
    assert_eq!(urls[5], format!("{base}/PROJ/repos/lib/default-branch"));
    assert_eq!(urls[6], format!("{base}/PROJ/repos/lib/raw/b.md?at=main"));
    assert!(requests.iter().all(|request| !request.effect));
}

#[tokio::test]
async fn the_same_repository_by_another_spelling_is_the_toolkits_own_and_bad_segments_are_refused()
{
    let (client, transport) = client(
        server_config(),
        [BitbucketHttpResponse::text(StatusCode::OK, "app text")],
    );
    let read = BitbucketOperation::ReadFile {
        file_path: "a.md",
        branch: None,
        start_line: None,
        end_line: None,
    };
    let same = client
        .execute_for(&target(Some("proj"), Some("APP")), read)
        .await
        .expect("same repository");
    assert_eq!(text(&same), "app text");
    assert_eq!(transport.requests.lock().expect("requests").len(), 1);
    for bad in ["../x", "a/b", "{not hex}", "a b", ".hidden"] {
        let refused = client
            .execute_for(
                &target(Some(bad), Some("lib")),
                BitbucketOperation::GetPullRequest { pr_id: 1 },
            )
            .await
            .expect_err("invalid segment");
        assert_eq!(
            refused.code(),
            BitbucketClientErrorCode::InvalidInput,
            "{bad}"
        );
    }
    // A Cloud workspace may be its {uuid}.
    let (cloud, transport) = client(
        cloud_config(),
        [
            ok(json!({"mainbranch": {"name": "main"}})),
            ok(json!({"id": 1})),
        ],
    );
    cloud
        .execute_for(
            &target(Some("{4f1a-90bc}"), Some("lib")),
            BitbucketOperation::GetPullRequest { pr_id: 1 },
        )
        .await
        .expect("uuid workspace");
    let requests = transport.requests.lock().expect("requests").clone();
    assert!(
        requests[0]
            .url
            .ends_with("/2.0/repositories/%7B4f1a-90bc%7D/lib")
    );
    assert!(
        requests[1]
            .url
            .ends_with("/repositories/%7B4f1a-90bc%7D/lib/pullrequests/1")
    );
}

//! The effectful GitHub tools against a scripted fake GitHub: what each one
//! sends, what it answers, and which sentence each failure becomes.
//!
//! The fake is a `Transport`, so the real client, the real request builder
//! and the real status mapping run; only the network is replaced. A request
//! the script does not expect fails the test, so a tool that sends a write it
//! should not (a PUT over an existing file, a DELETE after a refused read)
//! cannot pass.
#![allow(
    clippy::needless_pass_by_value,
    reason = "test helpers take the JSON they were handed"
)]

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use elitea_connectors::transport::header::{HeaderName, HeaderValue};
use elitea_connectors::transport::{
    HeaderMap, Request, Response, StatusCode, Transport, TransportError,
};
use serde_json::{Map, Value, json};

use super::families::github::client::{GitHubApi, GitHubClient};
use super::families::github::config::GitHubToolkitConfig;
use super::families::github::tools::{WriteBinding, test_build_with_writer};
use super::families::office_host::{
    OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost,
};
use super::policy::ToolAdmissionPolicy;

const REPO: &str = "/api/v3/repos/EliteaAI/elitea-platform";

fn sha(character: char) -> String {
    character.to_string().repeat(40)
}

// ----- the fake GitHub ---------------------------------------------------------

#[derive(Clone)]
enum Reply {
    Json(u16, Value),
    Limited(u16, Value),
    Transport(TransportError),
}

#[derive(Debug, Clone)]
struct Hit {
    method: String,
    path: String,
    query: String,
    body: Option<Value>,
}

#[derive(Default)]
struct Script {
    routes: Mutex<BTreeMap<String, VecDeque<Reply>>>,
    /// GraphQL answers by a fragment of the document, first match wins.
    graph: Mutex<Vec<(String, VecDeque<Reply>)>>,
    hits: Mutex<Vec<Hit>>,
}

impl Script {
    /// The next reply to `METHOD path` (the last one repeats).
    fn on(&self, method: &str, path: &str, reply: Reply) -> &Self {
        self.routes
            .lock()
            .expect("routes")
            .entry(format!("{method} {REPO}{path}"))
            .or_default()
            .push_back(reply);
        self
    }

    /// The next answer to a GraphQL document containing `fragment`.
    fn on_graph(&self, fragment: &str, reply: Reply) -> &Self {
        let mut graph = self.graph.lock().expect("graph");
        if let Some((_, queue)) = graph.iter_mut().find(|(known, _)| known == fragment) {
            queue.push_back(reply);
        } else {
            graph.push((fragment.to_owned(), VecDeque::from([reply])));
        }
        drop(graph);
        self
    }

    fn data(&self, fragment: &str, data: Value) -> &Self {
        self.on_graph(fragment, Reply::Json(200, json!({"data": data})))
    }

    /// The GraphQL operations sent, by document name, with their variables.
    fn operations(&self) -> Vec<(String, Value)> {
        self.hits()
            .into_iter()
            .filter(|hit| hit.path == "/api/graphql")
            .map(|hit| {
                let body = hit.body.expect("a GraphQL body");
                let query = body["query"].as_str().expect("query");
                let name = query
                    .split_whitespace()
                    .nth(1)
                    .map(|word| word.split('(').next().unwrap_or(word).to_owned())
                    .expect("an operation name");
                (name, body["variables"].clone())
            })
            .collect()
    }

    fn json(&self, method: &str, path: &str, status: u16, body: Value) -> &Self {
        self.on(method, path, Reply::Json(status, body))
    }

    fn hits(&self) -> Vec<Hit> {
        self.hits.lock().expect("hits").clone()
    }

    /// The writes sent: everything that is not a GET.
    fn writes(&self) -> Vec<Hit> {
        self.hits()
            .into_iter()
            .filter(|hit| hit.method != "GET")
            .collect()
    }
}

#[async_trait]
impl Transport for Script {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        let method = request.method().to_string();
        let path = request.url().path().to_owned();
        let body = request
            .body()
            .and_then(|body| body.as_bytes())
            .and_then(|bytes| serde_json::from_slice::<Value>(bytes).ok());
        self.hits.lock().expect("hits").push(Hit {
            method: method.clone(),
            path: path.clone(),
            query: request.url().query().unwrap_or_default().to_owned(),
            body,
        });
        let key = format!("{method} {path}");
        let graph_reply = if path == "/api/graphql" {
            let query = self
                .hits
                .lock()
                .expect("hits")
                .last()
                .and_then(|hit| hit.body.as_ref())
                .and_then(|body| body["query"].as_str().map(str::to_owned))
                .unwrap_or_default();
            let mut graph = self.graph.lock().expect("graph");
            let Some((_, queue)) = graph
                .iter_mut()
                .find(|(fragment, _)| query.contains(fragment.as_str()))
            else {
                panic!("the fake GitHub has no GraphQL answer for {query}");
            };
            Some(if queue.len() > 1 {
                queue.pop_front().expect("a queued reply")
            } else {
                queue.front().cloned().expect("a reply")
            })
        } else {
            None
        };
        let reply = if let Some(reply) = graph_reply {
            reply
        } else {
            let mut routes = self.routes.lock().expect("routes");
            let Some(queue) = routes.get_mut(&key) else {
                panic!("the fake GitHub has no route for {key}");
            };
            if queue.len() > 1 {
                queue.pop_front().expect("a queued reply")
            } else {
                queue.front().cloned().expect("a reply")
            }
        };
        match reply {
            Reply::Transport(error) => Err(error),
            Reply::Json(status, body) => Ok(Response::from_bytes(
                StatusCode::from_u16(status).expect("status"),
                HeaderMap::new(),
                body.to_string().into_bytes(),
            )),
            Reply::Limited(status, body) => {
                let mut headers = HeaderMap::new();
                headers.insert(
                    HeaderName::from_static("x-ratelimit-remaining"),
                    HeaderValue::from_static("0"),
                );
                Ok(Response::from_bytes(
                    StatusCode::from_u16(status).expect("status"),
                    headers,
                    body.to_string().into_bytes(),
                ))
            }
        }
    }
}

// ----- the fake platform file store ---------------------------------------------

#[derive(Default)]
struct Store {
    objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    refuse: Mutex<Option<OfficeFailure>>,
}

#[async_trait]
impl OfficeToolHost for Store {
    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        if let Some(kind) = *self.refuse.lock().expect("refuse") {
            return Err(OfficeHostError::new(kind, "refused"));
        }
        let objects = self.objects.lock().expect("objects");
        let bytes = objects
            .get(&(bucket.to_owned(), name.to_owned()))
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))?;
        if bytes.len() as u64 > max_bytes {
            return Err(OfficeHostError::new(OfficeFailure::TooLarge, "big"));
        }
        Ok(bytes.clone())
    }

    async fn write_object(
        &self,
        _bucket: &str,
        _name: &str,
        _media_type: &str,
        _bytes: Vec<u8>,
        _overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Err(OfficeHostError::new(
            OfficeFailure::Unsupported,
            "no writes",
        ))
    }

    async fn complete(&self, _request: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NoModel, "no model"))
    }
}

// ----- the rig --------------------------------------------------------------------

struct Rig {
    tools: Vec<Arc<dyn Tool>>,
    client: Arc<GitHubClient>,
}

fn settings(selected: &[&str]) -> Map<String, Value> {
    json!({
        "github_configuration": {
            "configuration_type": "github",
            "base_url": "https://github.example.test/api/v3/",
            "access_token": "fixture-token"
        },
        "repository": "https://github.example.test/EliteaAI/elitea-platform.git",
        "active_branch": "feature/runtime",
        "base_branch": "main",
        "selected_tools": selected,
        "embedding_model": null
    })
    .as_object()
    .expect("settings")
    .clone()
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("github-writes-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

async fn rig_with(script: Arc<Script>, store: Option<Arc<Store>>) -> Rig {
    let config = GitHubToolkitConfig::parse(&settings(&[])).expect("config");
    let client =
        Arc::new(GitHubClient::test_with_transport(config, script.clone()).expect("client"));
    let api: Arc<dyn GitHubApi> = client.clone();
    let writer = WriteBinding {
        client: client.clone(),
        office: store.map(|store| store as Arc<dyn OfficeToolHost>),
    };
    let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"));
    let toolset = test_build_with_writer(
        "team-github",
        "EliteaAI/elitea-platform",
        &super::families::github::tools::test_served_names(),
        &policy,
        &api,
        &writer,
    )
    .expect("toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = adk_core::Toolset::tools(&toolset, readonly)
        .await
        .expect("tools");
    Rig { tools, client }
}

async fn rig(script: &Arc<Script>) -> Rig {
    rig_with(script.clone(), None).await
}

impl Rig {
    async fn call(&self, name: &str, arguments: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|tool| tool.name() == name)
            .unwrap_or_else(|| panic!("no tool {name}"));
        let context: Arc<dyn ToolContext> = context();
        tool.execute(context, arguments)
            .await
            .unwrap_or_else(|error| panic!("{name} returned an error: {error}"))
    }

    async fn says(&self, name: &str, arguments: Value) -> String {
        match self.call(name, arguments).await {
            Value::String(text) => text,
            other => panic!("{name} answered {other}, not text"),
        }
    }
}

fn file_json(content: &str, sha_char: char) -> Value {
    json!({
        "type": "file",
        "encoding": "base64",
        "size": content.len(),
        "sha": sha(sha_char),
        "content": STANDARD.encode(content),
    })
}

fn body_of(hit: &Hit) -> &Value {
    hit.body.as_ref().expect("a request body")
}

// ----- classification -----------------------------------------------------------------

#[tokio::test]
async fn the_effects_are_not_read_only_and_the_reads_are() {
    let script = Arc::new(Script::default());
    let rig = rig(&script).await;
    let effects = [
        "apply_git_patch",
        "apply_git_patch_from_file",
        "comment_on_issue",
        "create_branch",
        "create_file",
        "create_issue",
        "create_issue_on_project",
        "create_pull_request",
        "delete_branch",
        "delete_file",
        "set_active_branch",
        "trigger_workflow",
        "update_file",
        "update_issue",
        "update_issue_on_project",
    ];
    for tool in &rig.tools {
        let is_effect = effects.contains(&tool.name());
        assert_eq!(tool.is_read_only(), !is_effect, "{}", tool.name());
        if is_effect {
            assert!(!tool.is_concurrency_safe(), "{}", tool.name());
        }
    }
    assert_eq!(rig.tools.len(), 37);
    assert!(script.hits().is_empty(), "building tools sends nothing");
}

// ----- branches -----------------------------------------------------------------------------

#[tokio::test]
async fn set_active_branch_moves_the_branch_the_reads_default_to() {
    let script = Arc::new(Script::default());
    script.json("GET", "/branches/dev", 200, json!({"name": "dev"}));
    script.json(
        "GET",
        "/branches/ghost",
        404,
        json!({"message": "Branch not found"}),
    );
    script.json("GET", "/branches/locked", 403, json!({"message": "no"}));
    let rig = rig(&script).await;
    assert_eq!(rig.client.active_branch(), "feature/runtime");

    let says = rig
        .says("set_active_branch", json!({"branch_name": "dev"}))
        .await;
    assert_eq!(says, "Active branch set to 'dev'");
    assert_eq!(rig.client.active_branch(), "dev");

    let says = rig
        .says("set_active_branch", json!({"branch_name": "ghost"}))
        .await;
    assert_eq!(says, "Branch 'ghost' not found in repository");
    assert_eq!(
        rig.client.active_branch(),
        "dev",
        "a missing branch changes nothing"
    );

    let says = rig
        .says("set_active_branch", json!({"branch_name": "locked"}))
        .await;
    assert!(
        says.starts_with("Could not set the active branch: GitHub refused (403)"),
        "{says}"
    );
    assert_eq!(rig.client.active_branch(), "dev");

    // An external repository has no active branch of its own.
    let before = script.hits().len();
    let says = rig
        .says(
            "set_active_branch",
            json!({"branch_name": "dev", "repo_name": "other/repo"}),
        )
        .await;
    assert!(
        says.starts_with("Cannot set active branch for external repository."),
        "{says}"
    );
    assert_eq!(script.hits().len(), before, "nothing was sent");
}

#[tokio::test]
async fn create_branch_starts_from_the_active_branch_and_becomes_active() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/branches/feature%2Fruntime",
        200,
        json!({"commit": {"sha": sha('c')}}),
    );
    script.json("POST", "/git/refs", 201, json!({"ref": "refs/heads/work"}));
    let rig = rig(&script).await;

    let says = rig
        .says("create_branch", json!({"proposed_branch_name": "work"}))
        .await;
    assert_eq!(
        says,
        "Branch 'work' created successfully, and set as current active branch."
    );
    assert_eq!(rig.client.active_branch(), "work");
    let writes = script.writes();
    assert_eq!(writes.len(), 1);
    assert_eq!(writes[0].method, "POST");
    assert_eq!(
        body_of(&writes[0]),
        &json!({"ref": "refs/heads/work", "sha": sha('c')})
    );
}

#[tokio::test]
async fn a_taken_branch_name_gets_the_first_free_suffix_without_walking_the_names() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/branches/feature%2Fruntime",
        200,
        json!({"commit": {"sha": sha('c')}}),
    );
    // work is taken, so are work_v1 and work_v4: the listing says _v2 is the
    // first free name, a concurrent writer takes it, and _v3 is free.
    let taken = json!({"message": "Reference already exists"});
    script.json("POST", "/git/refs", 422, taken.clone());
    script.json("POST", "/git/refs", 422, taken);
    script.json("POST", "/git/refs", 201, json!({}));
    script.json(
        "GET",
        "/git/matching-refs/heads/work_v",
        200,
        json!([{"ref": "refs/heads/work_v1"}, {"ref": "refs/heads/work_v4"}]),
    );
    let rig = rig(&script).await;
    let says = rig
        .says("create_branch", json!({"proposed_branch_name": "work"}))
        .await;
    // The answer names the branch that exists.
    assert!(says.contains("created successfully"), "{says}");
    let posts: Vec<_> = script
        .writes()
        .iter()
        .map(|hit| body_of(hit)["ref"].as_str().expect("ref").to_owned())
        .collect();
    assert_eq!(
        posts,
        [
            "refs/heads/work",
            "refs/heads/work_v2",
            "refs/heads/work_v3"
        ]
    );
    assert_eq!(rig.client.active_branch(), "work_v3");
    assert!(says.contains("'work_v3'"), "{says}");
}

#[tokio::test]
async fn create_branch_failures_are_told_apart() {
    for (reply, expected) in [
        (
            Reply::Json(401, json!({"message": "Bad credentials"})),
            "rejected the toolkit's credentials (401)",
        ),
        (
            Reply::Json(403, json!({"message": "Resource not accessible"})),
            "GitHub refused (403)",
        ),
        (
            Reply::Limited(403, json!({"message": "rate limit"})),
            "rate limited",
        ),
        (
            Reply::Json(500, json!({})),
            "the change may or may not have been made",
        ),
        (
            Reply::Transport(TransportError::timeout()),
            "the change may or may not have been made",
        ),
        (
            Reply::Transport(TransportError::connect()),
            "Nothing was changed",
        ),
    ] {
        let script = Arc::new(Script::default());
        script.json(
            "GET",
            "/branches/feature%2Fruntime",
            200,
            json!({"commit": {"sha": sha('c')}}),
        );
        script.on("POST", "/git/refs", reply);
        let rig = rig(&script).await;
        let says = rig
            .says("create_branch", json!({"proposed_branch_name": "work"}))
            .await;
        assert!(says.contains(expected), "{expected}: {says}");
        assert_eq!(rig.client.active_branch(), "feature/runtime", "{expected}");
    }
    // A name GitHub refuses is told with GitHub's reason.
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/branches/feature%2Fruntime",
        200,
        json!({"commit": {"sha": sha('c')}}),
    );
    script.json(
        "POST",
        "/git/refs",
        422,
        json!({"message": "Reference name is invalid"}),
    );
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_branch",
            json!({"proposed_branch_name": "bad..name"}),
        )
        .await;
    assert_eq!(
        says,
        "Unable to create branch due to error: Reference name is invalid"
    );
}

#[tokio::test]
async fn delete_branch_protects_the_base_the_defaults_and_the_active_branch() {
    let script = Arc::new(Script::default());
    script.json("DELETE", "/git/refs/heads/old", 204, json!(null));
    script.json(
        "DELETE",
        "/git/refs/heads/feature/runtime",
        204,
        json!(null),
    );
    script.json(
        "DELETE",
        "/git/refs/heads/gone",
        404,
        json!({"message": "Reference does not exist"}),
    );
    script.json(
        "DELETE",
        "/git/refs/heads/odd",
        422,
        json!({"message": "Cannot delete it"}),
    );
    let rig = rig(&script).await;

    for protected in ["main", "MASTER", "Main"] {
        let says = rig
            .says("delete_branch", json!({"branch_name": protected}))
            .await;
        assert!(
            says.starts_with(&format!(
                "Cannot delete branch '{protected}': It is a protected branch"
            )),
            "{says}"
        );
    }
    let says = rig
        .says("delete_branch", json!({"branch_name": "feature/runtime"}))
        .await;
    assert!(says.contains("It is currently the active branch"), "{says}");
    assert!(script.writes().is_empty(), "a refusal sends nothing");

    assert_eq!(
        rig.says("delete_branch", json!({"branch_name": "old"}))
            .await,
        "Branch 'old' deleted successfully."
    );
    assert_eq!(
        rig.says("delete_branch", json!({"branch_name": "gone"}))
            .await,
        "Branch 'gone' not found in the repository."
    );
    assert_eq!(
        rig.says("delete_branch", json!({"branch_name": "odd"}))
            .await,
        "Cannot delete branch 'odd': Cannot delete it"
    );
    // Forced, the active branch goes and the base becomes active.
    assert_eq!(
        rig.says(
            "delete_branch",
            json!({"branch_name": "feature/runtime", "force": true})
        )
        .await,
        "Branch 'feature/runtime' deleted successfully. Active branch has been reset to 'main'."
    );
    assert_eq!(rig.client.active_branch(), "main");
}

// ----- files ------------------------------------------------------------------------------------

#[tokio::test]
async fn create_file_never_replaces_and_refuses_the_base_branch() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/src/new.txt",
        404,
        json!({"message": "Not Found"}),
    );
    script.json("GET", "/contents/src/old.txt", 200, file_json("old", 'a'));
    script.json("PUT", "/contents/src/new.txt", 201, json!({"commit": {}}));
    let rig = rig(&script).await;

    // The arguments the SDK refuses.
    assert!(
        rig.says(
            "create_file",
            json!({"file_path": "src/new.txt", "branch": "work"})
        )
        .await
        .starts_with("Must provide either 'file_contents'")
    );
    assert!(
        rig.says(
            "create_file",
            json!({"file_path": "src/new.txt", "branch": "work", "file_contents": "x", "filepath": "/b/f"})
        )
        .await
        .starts_with("Cannot provide both")
    );
    assert!(
        rig.says(
            "create_file",
            json!({"file_path": "src/new.txt", "branch": "main", "file_contents": "x"})
        )
        .await
        .contains("which is protected")
    );
    assert!(script.hits().is_empty(), "refusals send nothing");

    // An existing file is never overwritten: no PUT is sent.
    let says = rig
        .says(
            "create_file",
            json!({"file_path": "src/old.txt", "branch": "work", "file_contents": "new"}),
        )
        .await;
    assert_eq!(
        says,
        "File src/old.txt already exists. Use update_file instead."
    );
    assert!(script.writes().is_empty());

    let says = rig
        .says(
            "create_file",
            json!({"file_path": "src/new.txt", "branch": "work", "file_contents": "héllo\n"}),
        )
        .await;
    assert_eq!(says, "Created file src/new.txt");
    let writes = script.writes();
    assert_eq!(writes.len(), 1);
    let body = body_of(&writes[0]);
    assert_eq!(body["message"], "Create src/new.txt");
    assert_eq!(body["branch"], "work");
    assert_eq!(body["content"], STANDARD.encode("héllo\n"));
    assert!(body.get("sha").is_none(), "a create sends no sha");
}

#[tokio::test]
async fn create_file_failures_are_told_apart() {
    for (reply, expected) in [
        (
            Reply::Json(
                422,
                json!({"message": "Invalid request.\n\n\"sha\" wasn't supplied."}),
            ),
            "rejected the request as invalid (422). GitHub said: Invalid request.",
        ),
        (
            Reply::Json(409, json!({"message": "is at abc but expected def"})),
            "reports a conflict (409)",
        ),
        (
            Reply::Json(404, json!({"message": "Branch work not found"})),
            "did not find the repository, branch, file",
        ),
        (Reply::Json(401, json!({})), "(401)"),
        (Reply::Json(403, json!({})), "(403)"),
        (Reply::Limited(403, json!({})), "rate limited"),
        (Reply::Json(502, json!({})), "may or may not have been made"),
    ] {
        let script = Arc::new(Script::default());
        script.json("GET", "/contents/a.txt", 404, json!({}));
        script.on("PUT", "/contents/a.txt", reply);
        let rig = rig(&script).await;
        let says = rig
            .says(
                "create_file",
                json!({"file_path": "a.txt", "branch": "work", "file_contents": "x"}),
            )
            .await;
        assert!(says.contains(expected), "{expected}: {says}");
    }
    // A failing existence check stops before the write.
    let script = Arc::new(Script::default());
    script.json("GET", "/contents/a.txt", 403, json!({}));
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_file",
            json!({"file_path": "a.txt", "branch": "work", "file_contents": "x"}),
        )
        .await;
    assert!(says.contains("(403)"), "{says}");
    assert!(script.writes().is_empty());
}

#[tokio::test]
async fn create_file_copies_an_artifact_byte_for_byte() {
    let script = Arc::new(Script::default());
    script.json("GET", "/contents/img/logo.png", 404, json!({}));
    script.json("PUT", "/contents/img/logo.png", 201, json!({}));
    let store = Arc::new(Store::default());
    let bytes = vec![0x89, b'P', b'N', b'G', 0, 255, 1];
    store
        .objects
        .lock()
        .expect("objects")
        .insert(("shots".to_owned(), "logo.png".to_owned()), bytes.clone());
    let rig = rig_with(script.clone(), Some(store.clone())).await;
    let says = rig
        .says(
            "create_file",
            json!({"file_path": "img/logo.png", "branch": "work", "filepath": "/shots/logo.png"}),
        )
        .await;
    assert_eq!(says, "Created file img/logo.png (copied from artifact)");
    let writes = script.writes();
    assert_eq!(body_of(&writes[0])["content"], STANDARD.encode(&bytes));

    // A missing object, a refused store, no store at all: each its own text,
    // and nothing is written.
    let before = script.writes().len();
    let says = rig
        .says(
            "create_file",
            json!({"file_path": "img/logo.png", "branch": "work", "filepath": "/shots/none.png"}),
        )
        .await;
    assert_eq!(
        says,
        "Failed to retrieve artifact '/shots/none.png': no such file or bucket."
    );
    *store.refuse.lock().expect("refuse") = Some(OfficeFailure::AccessRefused);
    let says = rig
        .says(
            "create_file",
            json!({"file_path": "img/logo.png", "branch": "work", "filepath": "/shots/logo.png"}),
        )
        .await;
    assert!(
        says.contains("the platform refused access to the file store"),
        "{says}"
    );
    assert_eq!(script.writes().len(), before);

    let bare = rig_with(Arc::new(Script::default()), None).await;
    let says = bare
        .says(
            "create_file",
            json!({"file_path": "img/logo.png", "branch": "work", "filepath": "/shots/logo.png"}),
        )
        .await;
    assert!(says.starts_with("EliteA client not configured."), "{says}");
}

const EDIT: &str = "docs/a.md\nOLD <<<<\nhello world\n>>>> OLD\nNEW <<<<\nhello mars\n>>>> NEW";

#[tokio::test]
async fn update_file_edits_the_version_it_read_and_sends_that_sha() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/docs/a.md",
        200,
        file_json("# Doc\nhello world\n", 'a'),
    );
    script.json("PUT", "/contents/docs/a.md", 200, json!({}));
    let rig = rig(&script).await;
    let says = rig
        .says("update_file", json!({"file_query": EDIT, "branch": "work"}))
        .await;
    assert_eq!(says, "Updated file docs/a.md");
    let writes = script.writes();
    let body = body_of(&writes[0]);
    assert_eq!(body["sha"], sha('a'), "the sha that was read");
    assert_eq!(body["content"], STANDARD.encode("# Doc\nhello mars\n"));
    assert_eq!(body["message"], "Update docs/a.md");
    assert_eq!(body["branch"], "work");
    let reads: Vec<_> = script
        .hits()
        .into_iter()
        .filter(|hit| hit.method == "GET")
        .collect();
    assert_eq!(
        reads[0].query, "ref=work",
        "the file is read on the branch it is edited on"
    );

    // A custom commit message is used.
    rig.says(
        "update_file",
        json!({"file_query": EDIT, "branch": "work", "commit_message": "Fix greeting"}),
    )
    .await;
    assert_eq!(body_of(&script.writes()[1])["message"], "Fix greeting");
}

#[tokio::test]
async fn update_file_refuses_without_writing() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/docs/a.md",
        200,
        file_json("one\none\n", 'a'),
    );
    script.json("GET", "/contents/docs/missing.md", 404, json!({}));
    script.json(
        "GET",
        "/contents/docs/bin.md",
        200,
        json!({
            "type": "file", "encoding": "base64", "size": 2, "sha": sha('b'),
            "content": STANDARD.encode([0xff, 0xfe]),
        }),
    );
    let rig = rig(&script).await;
    let case =
        |path: &str, old: &str| format!("{path}\nOLD <<<<\n{old}\n>>>> OLD\nNEW <<<<\nx\n>>>> NEW");
    // The base branch.
    assert!(
        rig.says("update_file", json!({"file_query": EDIT, "branch": "main"}))
            .await
            .contains("which is protected")
    );
    // No path / no markers / binary extension.
    assert!(
        rig.says(
            "update_file",
            json!({"file_query": "\n  \n", "branch": "work"})
        )
        .await
        .starts_with("Invalid file_query format.")
    );
    assert!(
        rig.says(
            "update_file",
            json!({"file_query": "docs/a.md\nno markers", "branch": "work"})
        )
        .await
        .contains("No OLD/NEW marker pairs found")
    );
    assert!(
        rig.says(
            "update_file",
            json!({"file_query": case("a.png", "x"), "branch": "work"})
        )
        .await
        .contains("Cannot edit binary/document file 'a.png'")
    );
    // The OLD block matches twice / not at all.
    assert_eq!(
        rig.says(
            "update_file",
            json!({"file_query": case("docs/a.md", "one"), "branch": "work"})
        )
        .await,
        "Multiple candidate regions for OLD block in docs/a.md; no change applied to avoid ambiguity."
    );
    assert_eq!(
        rig.says(
            "update_file",
            json!({"file_query": case("docs/a.md", "two"), "branch": "work"})
        )
        .await,
        "Normalized OLD block not found in docs/a.md."
    );
    // A file that is not there, and one that is not text.
    assert!(
        rig.says(
            "update_file",
            json!({"file_query": case("docs/missing.md", "x"), "branch": "work"})
        )
        .await
        .contains("not found on branch work")
    );
    assert!(
        rig.says(
            "update_file",
            json!({"file_query": case("docs/bin.md", "x"), "branch": "work"})
        )
        .await
        .contains("is not UTF-8 text")
    );
    assert!(script.writes().is_empty(), "no refusal wrote anything");
}

#[tokio::test]
async fn a_file_changed_since_it_was_read_is_a_conflict_not_a_lost_update() {
    for reply in [
        Reply::Json(409, json!({"message": "docs/a.md does not match abc"})),
        Reply::Json(422, json!({"message": "sha does not match"})),
    ] {
        let script = Arc::new(Script::default());
        script.json(
            "GET",
            "/contents/docs/a.md",
            200,
            file_json("hello world\n", 'a'),
        );
        script.on("PUT", "/contents/docs/a.md", reply);
        let rig = rig(&script).await;
        let says = rig
            .says("update_file", json!({"file_query": EDIT, "branch": "work"}))
            .await;
        assert!(says.contains("reports a conflict (409)"), "{says}");
        assert!(
            says.contains("Read it again and redo the change."),
            "{says}"
        );
    }
}

#[tokio::test]
async fn delete_file_sends_the_sha_it_read_and_refuses_the_rest() {
    let script = Arc::new(Script::default());
    script.json("GET", "/contents/old.txt", 200, file_json("x", 'd'));
    script.json("GET", "/contents/gone.txt", 404, json!({}));
    script.json("GET", "/contents/dir", 200, json!([{"name": "a"}]));
    script.json("DELETE", "/contents/old.txt", 200, json!({}));
    let rig = rig(&script).await;
    assert!(
        rig.says(
            "delete_file",
            json!({"file_path": "old.txt", "branch": "main"})
        )
        .await
        .contains("which is protected")
    );
    assert_eq!(
        rig.says(
            "delete_file",
            json!({"file_path": "gone.txt", "branch": "work"})
        )
        .await,
        "File gone.txt not found."
    );
    assert!(
        rig.says("delete_file", json!({"file_path": "dir", "branch": "work"}))
            .await
            .contains("is a directory, not a file")
    );
    assert!(script.writes().is_empty());
    assert_eq!(
        rig.says(
            "delete_file",
            json!({"file_path": "old.txt", "branch": "work"})
        )
        .await,
        "Deleted file old.txt"
    );
    let writes = script.writes();
    assert_eq!(
        body_of(&writes[0]),
        &json!({"message": "Delete old.txt", "sha": sha('d'), "branch": "work"})
    );
}

// ----- pull requests, issues, comments, workflows -----------------------------------------------

#[tokio::test]
async fn create_pull_request_defaults_the_base_and_names_what_github_refused() {
    let script = Arc::new(Script::default());
    script.json(
        "POST",
        "/pulls",
        201,
        json!({"number": 7, "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/7"}),
    );
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_pull_request",
            json!({"title": "Add x", "body": "Why", "head": "work"}),
        )
        .await;
    assert_eq!(
        says,
        "Pull request created successfully! PR #7, URL: https://github.example.test/EliteaAI/elitea-platform/pull/7"
    );
    assert_eq!(
        body_of(&script.writes()[0]),
        &json!({"title": "Add x", "body": "Why", "head": "work", "base": "main"})
    );
    // An explicit base is used; a null one falls back.
    rig.says(
        "create_pull_request",
        json!({"title": "t", "body": "b", "head": "work", "base": "develop"}),
    )
    .await;
    assert_eq!(body_of(&script.writes()[1])["base"], "develop");

    let script = Arc::new(Script::default());
    script.json(
        "POST",
        "/pulls",
        422,
        json!({"message": "Validation Failed", "errors": [{"message": "A pull request already exists for EliteaAI:work."}]}),
    );
    let rig = self::rig(&script).await;
    let says = rig
        .says(
            "create_pull_request",
            json!({"title": "t", "body": "b", "head": "work"}),
        )
        .await;
    assert!(
        says.contains("A pull request already exists for EliteaAI:work."),
        "{says}"
    );
    assert!(says.contains("Validation Failed"), "{says}");
}

#[tokio::test]
async fn create_issue_sends_empty_lists_when_none_are_given() {
    let script = Arc::new(Script::default());
    script.json(
        "POST",
        "/issues",
        201,
        json!({"number": 12, "html_url": "https://github.example.test/EliteaAI/elitea-platform/issues/12"}),
    );
    let rig = rig(&script).await;
    let says = rig.says("create_issue", json!({"title": "Bug"})).await;
    assert_eq!(
        says,
        "Issue created successfully! ID:12, URL: https://github.example.test/EliteaAI/elitea-platform/issues/12"
    );
    assert_eq!(
        body_of(&script.writes()[0]),
        &json!({"title": "Bug", "labels": [], "assignees": []})
    );
    rig.says(
        "create_issue",
        json!({"title": "Bug", "body": "## x", "labels": ["bug"], "assignees": ["octocat"]}),
    )
    .await;
    assert_eq!(
        body_of(&script.writes()[1]),
        &json!({"title": "Bug", "body": "## x", "labels": ["bug"], "assignees": ["octocat"]})
    );

    for (status, expected) in [
        (404, "(404)"),
        (410, "could not be understood"),
        (503, "may or may not"),
    ] {
        let script = Arc::new(Script::default());
        script.json("POST", "/issues", status, json!({"message": "x"}));
        let rig = self::rig(&script).await;
        let says = rig.says("create_issue", json!({"title": "Bug"})).await;
        assert!(says.contains(expected), "{status}: {says}");
    }
}

#[tokio::test]
async fn update_issue_leaves_labels_and_assignees_alone_unless_told() {
    let script = Arc::new(Script::default());
    script.json(
        "PATCH",
        "/issues/5",
        200,
        json!({"number": 5, "html_url": "https://github.example.test/EliteaAI/elitea-platform/issues/5"}),
    );
    let rig = rig(&script).await;
    // Closing an issue sends no labels and no assignees: GitHub keeps them.
    let says = rig
        .says("update_issue", json!({"issue_id": 5, "state": "closed"}))
        .await;
    assert!(
        says.starts_with("Issue updated successfully! Updated details: ID: 5, URL: https://github"),
        "{says}"
    );
    assert!(!says.contains("removed"), "{says}");
    assert_eq!(body_of(&script.writes()[0]), &json!({"state": "closed"}));

    // Given, they replace; an explicit empty list clears, and the answer says so.
    let says = rig
        .says(
            "update_issue",
            json!({"issue_id": 5, "title": "New", "body": "B", "labels": ["bug"], "assignees": ["octocat"]}),
        )
        .await;
    assert!(!says.contains("removed"), "{says}");
    assert_eq!(
        body_of(&script.writes()[1]),
        &json!({"labels": ["bug"], "assignees": ["octocat"], "title": "New", "body": "B"})
    );
    let says = rig
        .says(
            "update_issue",
            json!({"issue_id": 5, "labels": [], "assignees": []}),
        )
        .await;
    assert_eq!(
        body_of(&script.writes()[2]),
        &json!({"labels": [], "assignees": []})
    );
    assert!(
        says.ends_with(
            "An empty list was passed, so all labels and assignees were removed from the issue."
        ),
        "{says}"
    );
    let says = rig
        .says("update_issue", json!({"issue_id": 5, "labels": []}))
        .await;
    assert!(
        says.ends_with("so all labels were removed from the issue."),
        "{says}"
    );

    // Nothing to change sends nothing.
    let before = script.writes().len();
    let says = rig.says("update_issue", json!({"issue_id": 5})).await;
    assert!(says.starts_with("Nothing to update"), "{says}");
    let says = rig
        .says("update_issue", json!({"issue_id": 5, "state": "done"}))
        .await;
    assert_eq!(says, "`state` must be 'open' or 'closed'");
    assert_eq!(script.writes().len(), before);

    let script = Arc::new(Script::default());
    script.json("PATCH", "/issues/99", 404, json!({"message": "Not Found"}));
    let rig = self::rig(&script).await;
    let says = rig
        .says("update_issue", json!({"issue_id": 99, "title": "x"}))
        .await;
    assert!(says.contains("(404)"), "{says}");
}

/// The SDK's pydantic models read "5" as 5; anything that is not a number is
/// told so, and nothing is sent.
#[tokio::test]
async fn integer_ids_are_read_the_way_the_sdk_reads_them() {
    let script = Arc::new(Script::default());
    script.json(
        "PATCH",
        "/issues/5",
        200,
        json!({"number": 5, "html_url": "https://github.example.test/x/5"}),
    );
    let rig = rig(&script).await;
    for id in [json!("5"), json!(" 5 "), json!(5.0)] {
        let says = rig
            .says("update_issue", json!({"issue_id": id, "state": "open"}))
            .await;
        assert!(
            says.starts_with("Issue updated successfully!"),
            "{id}: {says}"
        );
    }
    for id in [
        json!("five"),
        json!("5x"),
        json!(""),
        json!(-5),
        json!(true),
        json!(5.5),
        json!([5]),
    ] {
        let says = rig
            .says(
                "update_issue",
                json!({"issue_id": id.clone(), "state": "open"}),
            )
            .await;
        assert_eq!(says, "`issue_id` must be a number", "{id}");
    }
    assert_eq!(
        script.writes().len(),
        3,
        "only the three readable ids were sent"
    );
}

#[tokio::test]
async fn comment_on_issue_takes_the_number_as_a_string() {
    let script = Arc::new(Script::default());
    script.json(
        "POST",
        "/issues/42/comments",
        201,
        json!({"html_url": "https://github.example.test/EliteaAI/elitea-platform/issues/42#issuecomment-1"}),
    );
    let rig = rig(&script).await;
    let says = rig
        .says(
            "comment_on_issue",
            json!({"issue_number": "42", "comment": "Looks good"}),
        )
        .await;
    assert_eq!(
        says,
        "Comment added successfully! URL: https://github.example.test/EliteaAI/elitea-platform/issues/42#issuecomment-1"
    );
    assert_eq!(body_of(&script.writes()[0]), &json!({"body": "Looks good"}));
    let says = rig
        .says(
            "comment_on_issue",
            json!({"issue_number": "forty-two", "comment": "x"}),
        )
        .await;
    assert!(
        says.contains("must be the issue or PR number as digits"),
        "{says}"
    );
    assert_eq!(script.writes().len(), 1);
}

#[tokio::test]
async fn trigger_workflow_resolves_the_workflow_and_dispatches_it_by_id() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/actions/workflows/build.yml",
        200,
        json!({"id": 1234, "name": "Build", "html_url": "https://github.example.test/EliteaAI/elitea-platform/actions/workflows/build.yml"}),
    );
    script.json(
        "GET",
        "/actions/workflows/nope.yml",
        404,
        json!({"message": "Not Found"}),
    );
    script.json(
        "POST",
        "/actions/workflows/1234/dispatches",
        204,
        json!(null),
    );
    let rig = rig(&script).await;
    let result = rig
        .call(
            "trigger_workflow",
            json!({"workflow_id": "build.yml", "ref": "main", "inputs": {"env": "stage"}}),
        )
        .await;
    assert_eq!(result["success"], true);
    assert_eq!(result["workflow_id"], 1234);
    assert_eq!(result["workflow_name"], "Build");
    assert_eq!(
        result["message"],
        "Workflow 'Build' triggered successfully on ref 'main'"
    );
    assert_eq!(result["inputs"], json!({"env": "stage"}));
    assert_eq!(
        body_of(&script.writes()[0]),
        &json!({"ref": "main", "inputs": {"env": "stage"}})
    );
    let says = rig
        .says(
            "trigger_workflow",
            json!({"workflow_id": "nope.yml", "ref": "main"}),
        )
        .await;
    assert_eq!(
        says,
        "Workflow with ID or filename 'nope.yml' not found in repository EliteaAI/elitea-platform"
    );
    assert_eq!(script.writes().len(), 1);

    // GitHub refuses a dispatch when the workflow has no workflow_dispatch trigger.
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/actions/workflows/ci.yml",
        200,
        json!({"id": 9, "name": "CI", "html_url": "https://github.example.test/x"}),
    );
    script.json(
        "POST",
        "/actions/workflows/9/dispatches",
        422,
        json!({"message": "Workflow does not have 'workflow_dispatch' trigger"}),
    );
    let rig = self::rig(&script).await;
    let says = rig
        .says(
            "trigger_workflow",
            json!({"workflow_id": "ci.yml", "ref": "main"}),
        )
        .await;
    assert!(
        says.contains("Workflow does not have 'workflow_dispatch' trigger"),
        "{says}"
    );
}

/// Every single-request effect maps the same statuses to the same causes:
/// the sentence differs by cause, never by tool, and no cause is silent.
#[tokio::test]
async fn every_simple_effect_maps_each_failure_to_its_own_sentence() {
    let causes: [(Reply, &str); 8] = [
        (
            Reply::Json(401, json!({})),
            "rejected the toolkit's credentials (401)",
        ),
        (Reply::Json(403, json!({})), "GitHub refused (403)"),
        (Reply::Limited(403, json!({})), "rate limited the request"),
        (
            Reply::Json(404, json!({})),
            "did not find the repository, branch, file, issue or workflow",
        ),
        (
            Reply::Json(409, json!({"message": "busy"})),
            "reports a conflict (409)",
        ),
        (
            Reply::Json(422, json!({"message": "Validation Failed"})),
            "rejected the request as invalid (422). GitHub said: Validation Failed",
        ),
        (Reply::Json(503, json!({})), "may or may not have been made"),
        (
            Reply::Transport(TransportError::connect()),
            "Nothing was changed",
        ),
    ];
    let calls: [(&str, &str, &str, Value); 4] = [
        (
            "create_pull_request",
            "POST",
            "/pulls",
            json!({"title": "t", "body": "b", "head": "work"}),
        ),
        ("create_issue", "POST", "/issues", json!({"title": "t"})),
        (
            "update_issue",
            "PATCH",
            "/issues/3",
            json!({"issue_id": 3, "title": "t"}),
        ),
        (
            "comment_on_issue",
            "POST",
            "/issues/3/comments",
            json!({"issue_number": "3", "comment": "c"}),
        ),
    ];
    for (tool, method, path, arguments) in calls {
        let mut seen = std::collections::BTreeSet::new();
        for (reply, expected) in &causes {
            let script = Arc::new(Script::default());
            script.on(method, path, reply.clone());
            let says = rig(&script).await.says(tool, arguments.clone()).await;
            assert!(says.contains(expected), "{tool}: {expected}: {says}");
            assert!(seen.insert(says), "{tool}: two causes read the same");
        }
    }
    // A workflow whose dispatch is refused is told the same way.
    for (reply, expected) in causes {
        let script = Arc::new(Script::default());
        script.json(
            "GET",
            "/actions/workflows/ci.yml",
            200,
            json!({"id": 9, "name": "CI", "html_url": "https://github.example.test/x"}),
        );
        script.on("POST", "/actions/workflows/9/dispatches", reply);
        let says = rig(&script)
            .await
            .says(
                "trigger_workflow",
                json!({"workflow_id": "ci.yml", "ref": "main"}),
            )
            .await;
        assert!(
            says.contains(expected),
            "trigger_workflow: {expected}: {says}"
        );
    }
}

// ----- patches ---------------------------------------------------------------------------------------

const PATCH_TWO_FILES: &str = "diff --git a/src/a.txt b/src/a.txt\n--- a/src/a.txt\n+++ b/src/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\ndiff --git a/src/new.txt b/src/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/src/new.txt\n@@ -0,0 +1 @@\n+fresh\n";

#[tokio::test]
async fn a_patch_is_checked_against_the_branch_before_anything_is_written() {
    let script = Arc::new(Script::default());
    // src/a.txt on the branch is not what the patch was made for.
    script.json(
        "GET",
        "/contents/src/a.txt",
        200,
        file_json("one\nOTHER\n", 'a'),
    );
    script.json("GET", "/contents/src/new.txt", 404, json!({}));
    let rig = rig(&script).await;
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": PATCH_TWO_FILES, "branch": "work"}),
        )
        .await;
    assert_eq!(result["success"], false);
    assert_eq!(result["successful_changes"], 0);
    assert_eq!(result["failed_count"], 1);
    assert!(
        result["failed_changes"][0]
            .as_str()
            .expect("sentence")
            .contains("does not match the file at line 2"),
        "{result}"
    );
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("nothing was changed"),
        "{result}"
    );
    assert!(
        script.writes().is_empty(),
        "no file was written, not even the one that fits"
    );
}

#[tokio::test]
async fn a_patch_that_fits_is_committed_file_by_file() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/src/a.txt",
        200,
        file_json("one\ntwo\n", 'a'),
    );
    script.json("GET", "/contents/src/new.txt", 404, json!({}));
    script.json("PUT", "/contents/src/a.txt", 200, json!({}));
    script.json("PUT", "/contents/src/new.txt", 201, json!({}));
    let rig = rig(&script).await;
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": PATCH_TWO_FILES, "branch": "work", "commit_message": "Sync"}),
        )
        .await;
    assert_eq!(result["success"], true);
    assert_eq!(
        result["applied_changes"],
        json!(["Modified: src/a.txt", "Created: src/new.txt"])
    );
    assert_eq!(result["failed_changes"], json!([]));
    assert_eq!(
        result["message"],
        "Patch successfully applied. 2 changes made."
    );
    let writes = script.writes();
    assert_eq!(body_of(&writes[0])["message"], "Sync - Update src/a.txt");
    assert_eq!(body_of(&writes[0])["sha"], sha('a'));
    assert_eq!(
        body_of(&writes[0])["content"],
        STANDARD.encode("one\nTWO\n")
    );
    assert_eq!(body_of(&writes[1])["message"], "Sync - Create src/new.txt");
    assert_eq!(body_of(&writes[1])["content"], STANDARD.encode("fresh\n"));
}

#[tokio::test]
async fn a_write_that_fails_midway_reports_what_was_applied_and_what_was_not_tried() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/src/a.txt",
        200,
        file_json("one\ntwo\n", 'a'),
    );
    script.json("GET", "/contents/src/new.txt", 404, json!({}));
    script.json(
        "PUT",
        "/contents/src/a.txt",
        403,
        json!({"message": "Resource not accessible"}),
    );
    let rig = rig(&script).await;
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": PATCH_TWO_FILES, "branch": "work"}),
        )
        .await;
    assert_eq!(result["success"], false);
    assert_eq!(result["applied_changes"], json!([]));
    let failed = result["failed_changes"].as_array().expect("list");
    assert_eq!(failed.len(), 2);
    assert!(
        failed[0]
            .as_str()
            .expect("s")
            .starts_with("Failed to apply modify src/a.txt: GitHub refused the write (403)")
    );
    assert_eq!(failed[1], "Not attempted: create src/new.txt");
    assert!(
        result["message"]
            .as_str()
            .expect("m")
            .starts_with("Patch partially applied. 0 successful, 2 failed or not attempted.")
    );
    assert_eq!(script.writes().len(), 1, "the second file was not tried");
}

#[tokio::test]
async fn deleted_and_renamed_files_and_the_refusals_of_a_patch() {
    let script = Arc::new(Script::default());
    script.json("GET", "/contents/gone.txt", 200, file_json("bye\n", 'd'));
    script.json("GET", "/contents/old.txt", 200, file_json("keep\n", 'e'));
    script.json("GET", "/contents/moved.txt", 404, json!({}));
    script.json("DELETE", "/contents/gone.txt", 200, json!({}));
    script.json("PUT", "/contents/moved.txt", 201, json!({}));
    script.json("DELETE", "/contents/old.txt", 200, json!({}));
    let rig = rig(&script).await;
    let patch = "diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\ndiff --git a/old.txt b/moved.txt\nsimilarity index 100%\nrename from old.txt\nrename to moved.txt\n";
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": patch, "branch": "work"}),
        )
        .await;
    assert_eq!(
        result["applied_changes"],
        json!(["Deleted: gone.txt", "Renamed: old.txt \u{2192} moved.txt"])
    );
    let order: Vec<_> = script
        .writes()
        .iter()
        .map(|hit| {
            format!(
                "{} {}",
                hit.method,
                hit.path.rsplit('/').next().unwrap_or_default()
            )
        })
        .collect();
    assert_eq!(
        order,
        ["DELETE gone.txt", "PUT moved.txt", "DELETE old.txt"],
        "a rename creates the new file before it removes the old one"
    );

    let before = script.hits().len();
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": patch, "branch": "main"}),
        )
        .await;
    assert_eq!(result["error"], "Cannot apply patch");
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": "not a diff", "branch": "work"}),
        )
        .await;
    assert_eq!(result["error"], "No valid changes found");
    assert_eq!(script.hits().len(), before, "neither refusal sent anything");
}

#[tokio::test]
async fn a_patch_from_a_bucket_reports_each_way_the_file_can_be_missing() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/src/a.txt",
        200,
        file_json("one\ntwo\n", 'a'),
    );
    script.json("PUT", "/contents/src/a.txt", 200, json!({}));
    let store = Arc::new(Store::default());
    let patch = "diff --git a/src/a.txt b/src/a.txt\n--- a/src/a.txt\n+++ b/src/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+2\n";
    store.objects.lock().expect("objects").insert(
        ("patches".to_owned(), "fix.patch".to_owned()),
        patch.as_bytes().to_vec(),
    );
    store.objects.lock().expect("objects").insert(
        ("patches".to_owned(), "bin.patch".to_owned()),
        vec![0xff, 0xfe],
    );
    let rig = rig_with(script.clone(), Some(store.clone())).await;
    let result = rig
        .call(
            "apply_git_patch_from_file",
            json!({"bucket_name": "patches", "file_name": "fix.patch", "branch": "work"}),
        )
        .await;
    assert_eq!(result["success"], true, "{result}");
    assert_eq!(
        body_of(&script.writes()[0])["message"],
        "Apply git patch from artifact - Update src/a.txt"
    );
    let result = rig
        .call(
            "apply_git_patch_from_file",
            json!({"bucket_name": "patches", "file_name": "none.patch", "branch": "work"}),
        )
        .await;
    assert_eq!(result["error"], "Patch file not found");
    assert_eq!(
        result["message"],
        "Patch file 'none.patch' not found in bucket 'patches'."
    );
    let result = rig
        .call(
            "apply_git_patch_from_file",
            json!({"bucket_name": "patches", "file_name": "bin.patch", "branch": "work"}),
        )
        .await;
    assert_eq!(result["error"], "Invalid patch content");
    *store.refuse.lock().expect("refuse") = Some(OfficeFailure::Unreachable);
    let result = rig
        .call(
            "apply_git_patch_from_file",
            json!({"bucket_name": "patches", "file_name": "fix.patch", "branch": "work"}),
        )
        .await;
    assert!(
        result["message"]
            .as_str()
            .expect("m")
            .contains("could not be reached"),
        "{result}"
    );
    let bare = rig_with(Arc::new(Script::default()), None).await;
    let result = bare
        .call(
            "apply_git_patch_from_file",
            json!({"bucket_name": "patches", "file_name": "fix.patch", "branch": "work"}),
        )
        .await;
    assert!(
        result["message"]
            .as_str()
            .expect("m")
            .contains("no access to the platform's file store")
    );
}

// ----- the two new reads ----------------------------------------------------------------------------------

#[tokio::test]
async fn workflow_logs_list_the_jobs_and_say_the_log_text_is_not_fetched() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/actions/runs/77",
        200,
        json!({"id": 77, "status": "completed", "conclusion": "failure"}),
    );
    script.json(
        "GET",
        "/actions/runs/77/jobs",
        200,
        json!({"total_count": 1, "jobs": [{
            "id": 5, "run_id": 77, "name": "build", "status": "completed", "conclusion": "failure",
            "url": "https://github.example.test/api/v3/repos/EliteaAI/elitea-platform/actions/jobs/5",
            "steps": [{"name": "Run tests", "status": "completed", "conclusion": "failure", "number": 2,
                       "started_at": "2026-10-01T10:00:00Z", "completed_at": "2026-10-01T10:01:00Z"}]
        }]}),
    );
    let rig = rig(&script).await;
    let result = rig.call("get_workflow_logs", json!({"run_id": "77"})).await;
    assert_eq!(result["run_id"], 77);
    assert_eq!(result["conclusion"], "failure");
    let job = &result["job_details"][0];
    assert_eq!(job["job_name"], "build");
    assert_eq!(job["steps"][0]["name"], "Run tests");
    assert!(
        job["logs_url"]
            .as_str()
            .expect("url")
            .ends_with("/actions/jobs/5/logs")
    );
    assert!(
        result["note"]
            .as_str()
            .expect("note")
            .contains("does not download the log archive")
    );
    assert_eq!(script.writes().len(), 0);
}

#[tokio::test]
async fn project_search_sends_the_query_to_github() {
    let script = Arc::new(Script::default());
    script.data(
        "EliteaProjectSearch",
        json!({"repository": {"projectV2": {
            "id": "P", "title": "Board", "url": "https://github.example.test/p",
            "fields": {"nodes": []},
            "items": {"totalCount": 1, "pageInfo": {"hasNextPage": false}, "nodes": [{
                "id": "I1", "type": "ISSUE", "fieldValues": {"nodes": []},
                "content": {"id": "C", "number": 3, "title": "Fix", "state": "OPEN", "url": "u", "body": "Details"}
            }]}
        }}}),
    );
    let rig = rig(&script).await;
    let result = rig
        .call(
            "search_project_issues",
            json!({"board_repo": "EliteaAI/board", "project_number": 4, "search_query": "status:Todo label:bug"}),
        )
        .await;
    assert_eq!(result["items"][0]["content"]["title"], "Fix");
    assert_eq!(result["items"][0]["content"]["body"], "Details");
    let hits = script.hits();
    let sent = body_of(&hits[0]);
    assert_eq!(sent["variables"]["searchQuery"], "status:Todo label:bug");
    assert_eq!(sent["variables"]["itemsCount"], 100);
    assert!(
        sent["query"]
            .as_str()
            .expect("query")
            .contains("items(first: $itemsCount, query: $searchQuery)"),
        "the filter is in the query itself"
    );
}

// ----- issues on a project board --------------------------------------------------------------------------

fn board_data() -> Value {
    json!({"repository": {
        "id": "REPO",
        "labels": {"nodes": [{"id": "L1", "name": "bug"}, {"id": "L2", "name": "ui"}]},
        "assignableUsers": {"nodes": [{"id": "U1", "name": "Dev Lead", "login": "dev_lead"}]},
        "projectsV2": {"nodes": [{
            "id": "PROJ", "title": "WebApp Redesign", "number": 4,
            "fields": {"nodes": [
                {"id": "F-STATUS", "dataType": "SINGLE_SELECT", "name": "Status",
                 "options": [{"id": "O-TODO", "name": "Todo"}, {"id": "O-DONE", "name": "Done"}]},
                {"id": "F-DUE", "dataType": "DATE", "name": "Due"},
                {"id": "F-LABELS", "dataType": "LABELS", "name": "Labels"},
                {"id": "F-ASSIGN", "dataType": "ASSIGNEES", "name": "Assignees"},
                {"id": "F-EST", "dataType": "NUMBER", "name": "Estimate"}
            ]}
        }]}
    }})
}

/// What GitHub answers for `repository(...).issue(number)`: the issue's node
/// id, the board items it is on (`(item id, project id)`) and its labels and
/// assignees.
fn issue_data(issue: &str, number: u64, on: &[(&str, &str)]) -> Value {
    let items: Vec<Value> = on
        .iter()
        .map(|(item, project)| json!({"id": item, "project": {"id": project}}))
        .collect();
    json!({"repository": {"issue": {
        "id": issue, "number": number,
        "projectItems": {"nodes": items, "pageInfo": {"hasNextPage": false}},
        "labels": {"nodes": [{"id": "L1", "name": "bug"}]},
        "assignees": {"nodes": [{"id": "U1", "name": "Dev Lead"}]}}}})
}

fn create_script() -> Arc<Script> {
    let script = Arc::new(Script::default());
    script.data("EliteaBoard(", board_data());
    script.data(
        "EliteaDraft",
        json!({"addProjectV2DraftIssue": {"projectItem": {"id": "DRAFT"}}}),
    );
    script.data(
        "EliteaConvert",
        json!({"convertProjectV2DraftIssueItemToIssue": {"item": {"id": "ITEM", "content": {"id": "ISSUE", "number": 42}}}}),
    );
    for fragment in [
        "EliteaSetField",
        "EliteaClearField",
        "EliteaAddLabels",
        "EliteaRemoveLabels",
        "EliteaAddAssignees",
        "EliteaRemoveAssignees",
    ] {
        script.data(fragment, json!({"ok": true}));
    }
    script
}

fn names(ops: &[(String, Value)]) -> Vec<&str> {
    ops.iter().map(|(name, _)| name.as_str()).collect()
}

#[tokio::test]
async fn an_issue_is_created_on_the_board_and_its_fields_are_set_in_order() {
    let script = create_script();
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_issue_on_project",
            json!({
                "board_repo": "EliteaAI/board", "project_title": "webapp redesign",
                "title": "Fix nav", "body": "Mobile",
                "fields": {"Status": "todo", "Due": "2026-11-01", "Labels": ["bug", "nope"],
                           "Assignees": ["dev_lead"], "Priority": "High", "Estimate": "3"}
            }),
        )
        .await;
    assert!(
        says.starts_with(
            "The issue with number '42' has been created.\nResponse on update fields: ["
        ),
        "{says}"
    );
    for set in ["'Status'", "'Due'", "'Labels'", "'Assignees'"] {
        assert!(says.contains(set), "{set}: {says}");
    }
    assert!(
        says.contains("{'field': 'Priority', 'reason': 'Field not found'}"),
        "{says}"
    );
    assert!(
        says.contains("{'field': 'Estimate', 'reason': 'Field type not supported'}"),
        "{says}"
    );

    let ops = script.operations();
    let order = names(&ops);
    assert_eq!(&order[..3], ["EliteaBoard", "EliteaDraft", "EliteaConvert"]);
    assert_eq!(
        ops[1].1,
        json!({"projectId": "PROJ", "title": "Fix nav", "body": "Mobile"})
    );
    assert_eq!(ops[2].1, json!({"itemId": "DRAFT", "repositoryId": "REPO"}));
    let rest: Vec<_> = ops[3..].iter().collect();
    let find = |name: &str, field: &str| {
        rest.iter()
            .find(|(op, vars)| {
                op == name && (vars["fieldId"] == field || vars.get("fieldId").is_none())
            })
            .map_or_else(|| panic!("no {name} for {field}"), |(_, vars)| vars.clone())
    };
    assert_eq!(
        find("EliteaSetField", "F-STATUS")["value"],
        json!({"singleSelectOptionId": "O-TODO"})
    );
    assert_eq!(
        find("EliteaSetField", "F-DUE")["value"],
        json!({"date": "2026-11-01T00:00:00Z"})
    );
    let labels = rest
        .iter()
        .find(|(op, _)| op == "EliteaAddLabels")
        .expect("labels");
    assert_eq!(
        labels.1,
        json!({"id": "ISSUE", "ids": ["L1"]}),
        "unknown labels are reported, not sent"
    );
    let assignees = rest
        .iter()
        .find(|(op, _)| op == "EliteaAddAssignees")
        .expect("assignees");
    assert_eq!(assignees.1, json!({"id": "ISSUE", "ids": ["U1"]}));
    // Item ids, not issue ids, address the field values.
    assert_eq!(find("EliteaSetField", "F-STATUS")["itemId"], "ITEM");
}

#[tokio::test]
async fn creating_an_issue_on_a_board_names_what_went_wrong_at_each_step() {
    let args = json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign", "title": "t", "body": "b"});
    // A missing board lists the boards there are.
    let script = create_script();
    let rig_ = rig(&script).await;
    let mut other = args.clone();
    other["project_title"] = json!("Roadmap");
    assert_eq!(
        rig_.says("create_issue_on_project", other).await,
        "Project 'Roadmap' not found. Available projects: ['WebApp Redesign']"
    );
    // A token without the project scope is told which scope to add.
    let script = Arc::new(Script::default());
    script.on_graph(
        "EliteaBoard(",
        Reply::Json(200, json!({"errors": [{"type": "INSUFFICIENT_SCOPES", "message": "Resource not accessible by personal access token"}]})),
    );
    let says = rig(&script)
        .await
        .says("create_issue_on_project", args.clone())
        .await;
    assert!(says.starts_with("GitHub Token Permission Error"), "{says}");
    assert!(says.contains("enable the 'project' scope"), "{says}");
    // A repo that is not owner/name.
    let mut bad = args.clone();
    bad["board_repo"] = json!("justname");
    assert!(
        rig(&create_script())
            .await
            .says("create_issue_on_project", bad)
            .await
            .contains("repo format is invalid")
    );

    // The draft fails: nothing else is sent.
    let script = Arc::new(Script::default());
    script.data("EliteaBoard(", board_data());
    script.on_graph(
        "EliteaDraft",
        Reply::Json(
            200,
            json!({"errors": [{"type": "UNPROCESSABLE", "message": "Title can't be blank"}]}),
        ),
    );
    let says = rig(&script)
        .await
        .says("create_issue_on_project", args.clone())
        .await;
    assert!(
        says.starts_with("Draft Issue Not Created. Error:"),
        "{says}"
    );
    assert!(says.contains("Title can't be blank"), "{says}");
    assert_eq!(names(&script.operations()), ["EliteaBoard", "EliteaDraft"]);

    // The conversion fails: the draft is left, and the answer says where.
    let script = Arc::new(Script::default());
    script.data("EliteaBoard(", board_data());
    script.data(
        "EliteaDraft",
        json!({"addProjectV2DraftIssue": {"projectItem": {"id": "DRAFT"}}}),
    );
    script.on_graph(
        "EliteaConvert",
        Reply::Json(
            200,
            json!({"errors": [{"type": "FORBIDDEN", "message": "no write access"}]}),
        ),
    );
    let says = rig(&script)
        .await
        .says("create_issue_on_project", args.clone())
        .await;
    assert!(says.starts_with("Convert Issue Failed. Error:"), "{says}");
    assert!(
        says.contains(
            "A draft item 'DRAFT' was added to the project 'WebApp Redesign' and was not converted"
        ),
        "{says}"
    );
    assert_eq!(
        names(&script.operations()),
        ["EliteaBoard", "EliteaDraft", "EliteaConvert"]
    );
}

#[tokio::test]
async fn a_field_that_fails_stops_the_rest_and_is_reported_with_what_was_set() {
    let script = create_script();
    script.on_graph(
        "EliteaClearField",
        Reply::Json(
            200,
            json!({"errors": [{"type": "FORBIDDEN", "message": "field is locked"}]}),
        ),
    );
    // Replace the generic success for clearing with the failing one.
    script
        .graph
        .lock()
        .expect("graph")
        .retain(|(fragment, _)| fragment != "EliteaClearField");
    script.on_graph(
        "EliteaClearField",
        Reply::Json(
            200,
            json!({"errors": [{"type": "FORBIDDEN", "message": "field is locked"}]}),
        ),
    );
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_issue_on_project",
            json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign", "title": "t", "body": "b",
                   "fields": {"Due": "", "Status": "Done"}}),
        )
        .await;
    assert!(
        says.starts_with("The issue with number '42' has been created."),
        "{says}"
    );
    assert!(
        says.contains(
            "The field 'Due' was NOT updated: Could not update the field: GitHub refused (403)"
        ),
        "{says}"
    );
    assert!(
        says.contains("Not attempted after it: ['Status']"),
        "{says}"
    );
    assert!(!names(&script.operations()).contains(&"EliteaSetField"));
}

#[tokio::test]
async fn an_issue_on_the_board_is_found_updated_and_its_fields_changed() {
    let script = create_script();
    script.data(
        "EliteaBoardIssue(",
        issue_data("ISSUE", 7, &[("ITEM", "PROJ")]),
    );
    script.data(
        "EliteaUpdateIssue",
        json!({"updateIssue": {"issue": {"id": "ISSUE", "number": 7}}}),
    );
    let rig = rig(&script).await;
    let says = rig
        .says(
            "update_issue_on_project",
            json!({"board_repo": "EliteaAI/board", "issue_number": "7", "project_title": "WebApp Redesign",
                   "title": "New title", "body": "New body",
                   "fields": {"Status": "Done", "Labels": [], "Assignees": []}}),
        )
        .await;
    assert!(
        says.starts_with(
            "The issue with number '7' has been updated.\nResponse on update fields: ["
        ),
        "{says}"
    );
    // One lookup of the issue itself: the board query, the issue, the update.
    let ops = script.operations();
    assert_eq!(ops[0].0, "EliteaBoard");
    assert_eq!(ops[1].0, "EliteaBoardIssue");
    assert_eq!(
        ops[1].1,
        json!({"owner": "EliteaAI", "name": "board", "number": 7})
    );
    assert_eq!(ops[2].0, "EliteaUpdateIssue");
    assert_eq!(
        ops[2].1,
        json!({"issueId": "ISSUE", "title": "New title", "body": "New body"})
    );
    // An empty list removes what the issue has now; nothing else is touched.
    let remove_labels = ops
        .iter()
        .find(|(name, _)| name == "EliteaRemoveLabels")
        .expect("remove labels");
    assert_eq!(remove_labels.1, json!({"id": "ISSUE", "ids": ["L1"]}));
    let remove_assignees = ops
        .iter()
        .find(|(name, _)| name == "EliteaRemoveAssignees")
        .expect("remove assignees");
    assert_eq!(remove_assignees.1, json!({"id": "ISSUE", "ids": ["U1"]}));
    let item_edit = ops
        .iter()
        .find(|(name, _)| name == "EliteaSetField")
        .expect("status");
    assert_eq!(item_edit.1["itemId"], "ITEM", "the item of THIS project");
    assert!(
        !script.hits()[0].body.as_ref().expect("body")["query"]
            .as_str()
            .expect("q")
            .contains("items("),
        "the board query carries no items"
    );
}

#[tokio::test]
async fn a_missing_or_off_board_issue_is_told_so_and_a_failed_title_writes_no_fields() {
    let args = json!({"board_repo": "EliteaAI/board", "issue_number": "99", "project_title": "WebApp Redesign",
                      "title": "t", "body": "b", "fields": {"Status": "Done"}});
    // Not in the board's repository, no issue_repo: say so and ask, never scan.
    let script = create_script();
    script.data("EliteaBoardIssue(", json!({"repository": {"issue": null}}));
    assert_eq!(
        rig(&script)
            .await
            .says("update_issue_on_project", args.clone())
            .await,
        "Issue number 99 was not found in EliteaAI/board, the repository of the board. If the issue is in another repository, pass issue_repo."
    );
    assert_eq!(
        names(&script.operations()),
        ["EliteaBoard", "EliteaBoardIssue"]
    );
    // Not found in the repository that was named.
    let script = create_script();
    script.data("EliteaIssueRepo", json!({"repository": {"id": "R2", "labels": {"nodes": []}, "assignableUsers": {"nodes": []}}}));
    script.data("EliteaBoardIssue(", json!({"repository": {"issue": null}}));
    let mut named = args.clone();
    named["issue_repo"] = json!("EliteaAI/other");
    assert_eq!(
        rig(&script)
            .await
            .says("update_issue_on_project", named)
            .await,
        "Issue number 99 was not found in EliteaAI/other."
    );
    // The issue exists but is on another board.
    let script = create_script();
    script.data(
        "EliteaBoardIssue(",
        issue_data("ISSUE", 99, &[("X", "OTHER-PROJECT")]),
    );
    assert_eq!(
        rig(&script)
            .await
            .says("update_issue_on_project", args.clone())
            .await,
        "Issue number 99 of EliteaAI/board is not on the project 'WebApp Redesign'."
    );
    assert!(!names(&script.operations()).contains(&"EliteaUpdateIssue"));
    // A bad number.
    let mut bad = args.clone();
    bad["issue_number"] = json!("nine");
    let script = create_script();
    assert_eq!(
        rig(&script)
            .await
            .says("update_issue_on_project", bad)
            .await,
        "issue_number must be a number such as '42'."
    );
    // The title and body are refused: the fields are not set.
    let script = create_script();
    script.data(
        "EliteaBoardIssue(",
        issue_data("ISSUE", 7, &[("ITEM", "PROJ")]),
    );
    script.on_graph(
        "EliteaUpdateIssue",
        Reply::Json(
            200,
            json!({"errors": [{"type": "FORBIDDEN", "message": "read only"}]}),
        ),
    );
    let mut found = args;
    found["issue_number"] = json!("7");
    let says = rig(&script)
        .await
        .says("update_issue_on_project", found)
        .await;
    assert!(
        says.starts_with("Issue title and body have not updated. Error:"),
        "{says}"
    );
    assert!(
        !names(&script.operations()).contains(&"EliteaSetField"),
        "fields are set only after the title and body"
    );
}

#[tokio::test]
async fn an_unreadable_answer_to_a_board_write_is_an_unknown_outcome() {
    let script = Arc::new(Script::default());
    script.data("EliteaBoard(", board_data());
    script.on_graph("EliteaDraft", Reply::Transport(TransportError::timeout()));
    let says = rig(&script)
        .await
        .says(
            "create_issue_on_project",
            json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign", "title": "t", "body": "b"}),
        )
        .await;
    assert!(says.contains("may or may not have been made"), "{says}");
}

// ----- review round 1 -----------------------------------------------------------------------------------

#[tokio::test]
async fn a_ref_name_with_a_dot_or_empty_segment_is_refused_before_any_request() {
    let script = Arc::new(Script::default());
    let rig = rig(&script).await;
    for name in [
        "./main",
        "../main",
        "a/../main",
        "a//b",
        "a/./b",
        "feature/",
        "/main",
        ".",
    ] {
        for (tool, arguments) in [
            ("delete_branch", json!({"branch_name": name})),
            ("set_active_branch", json!({"branch_name": name})),
            ("create_branch", json!({"proposed_branch_name": name})),
            (
                "create_file",
                json!({"file_path": "a.txt", "branch": name, "file_contents": "x"}),
            ),
            (
                "update_file",
                json!({"file_query": "a.md\nOLD <<<<\nx\n>>>> OLD\nNEW <<<<\ny\n>>>> NEW", "branch": name}),
            ),
            ("delete_file", json!({"file_path": "a.txt", "branch": name})),
            (
                "create_pull_request",
                json!({"title": "t", "body": "b", "head": name}),
            ),
            (
                "trigger_workflow",
                json!({"workflow_id": "ci.yml", "ref": name}),
            ),
        ] {
            let says = rig.says(tool, arguments).await;
            assert!(
                says.contains("must not contain empty, `.` or `..` segments"),
                "{tool} {name}: {says}"
            );
        }
    }
    assert!(
        script.hits().is_empty(),
        "nothing was sent, nothing deleted"
    );
    // A real nested branch is still fine.
    script.json("DELETE", "/git/refs/heads/feature/x", 204, json!(null));
    assert_eq!(
        rig.says("delete_branch", json!({"branch_name": "feature/x"}))
            .await,
        "Branch 'feature/x' deleted successfully."
    );
}

#[tokio::test]
async fn a_patch_path_with_a_dot_or_empty_segment_is_refused_before_any_request() {
    let script = Arc::new(Script::default());
    let rig = rig(&script).await;
    for path in ["../../etc/x", "a//b", "a/./b", "a/b/"] {
        let patch = format!(
            "diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n@@ -1 +1 @@\n-a\n+b\n"
        );
        let says = rig
            .says(
                "apply_git_patch",
                json!({"patch_content": patch, "branch": "work"}),
            )
            .await;
        assert!(
            says.contains("a file path in the patch") && says.contains("segments"),
            "{path}: {says}"
        );
    }
    assert!(script.hits().is_empty());
}

#[tokio::test]
async fn the_board_issue_is_looked_up_directly_in_the_repository_named() {
    // #42 exists in two repositories; each lookup names its own repository.
    let script = create_script();
    script.data(
        "EliteaBoardIssue(",
        issue_data("ISSUE-BOARD", 42, &[("ITEM-BOARD", "PROJ")]),
    );
    script.data(
        "EliteaBoardIssue(",
        issue_data("ISSUE-OTHER", 42, &[("ITEM-OTHER", "PROJ")]),
    );
    script.data(
        "EliteaIssueRepo",
        json!({"repository": {"id": "R2", "labels": {"nodes": []}, "assignableUsers": {"nodes": []}}}),
    );
    script.data(
        "EliteaUpdateIssue",
        json!({"updateIssue": {"issue": {"id": "x", "number": 42}}}),
    );
    let rig = rig(&script).await;
    let base = json!({"board_repo": "EliteaAI/board", "issue_number": "42",
        "project_title": "WebApp Redesign", "title": "t", "body": "b"});
    // The board's own repository by default.
    rig.says("update_issue_on_project", base.clone()).await;
    // issue_repo names the other one.
    let mut other = base;
    other["issue_repo"] = json!("EliteaAI/other");
    rig.says("update_issue_on_project", other).await;
    let ops = script.operations();
    let lookups: Vec<_> = ops
        .iter()
        .filter(|(name, _)| name == "EliteaBoardIssue")
        .collect();
    assert_eq!(lookups.len(), 2, "one request per update, no scan");
    assert_eq!(
        lookups[0].1,
        json!({"owner": "EliteaAI", "name": "board", "number": 42})
    );
    assert_eq!(
        lookups[1].1,
        json!({"owner": "EliteaAI", "name": "other", "number": 42})
    );
    let updates: Vec<_> = ops
        .iter()
        .filter(|(name, _)| name == "EliteaUpdateIssue")
        .collect();
    assert_eq!(updates[0].1["issueId"], "ISSUE-BOARD");
    assert_eq!(updates[1].1["issueId"], "ISSUE-OTHER");
}

#[tokio::test]
async fn board_lookups_map_as_reads_and_mutations_as_effects() {
    let args = json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign",
                      "title": "t", "body": "b"});
    for (kind, how) in [
        ("INTERNAL", "graphql"),
        ("SERVICE_UNAVAILABLE", "graphql"),
        ("", "http"),
    ] {
        // The lookup fails.
        let script = Arc::new(Script::default());
        let reply = if how == "http" {
            Reply::Json(503, json!({}))
        } else {
            Reply::Json(200, json!({"errors": [{"type": kind, "message": "boom"}]}))
        };
        script.on_graph("EliteaBoard(", reply);
        let says = rig(&script)
            .await
            .says("create_issue_on_project", args.clone())
            .await;
        assert!(!says.contains("may or may not"), "a read: {says}");
        assert!(
            says.contains("GitHub could not be reached") || says.contains("Failed"),
            "{says}"
        );
        assert!(
            says.contains("Nothing was changed") || says.contains("Failed to access"),
            "{says}"
        );
        assert_eq!(names(&script.operations()), ["EliteaBoard"]);
    }
    // The draft mutation fails with INTERNAL: it may have applied.
    for kind in ["INTERNAL", "SERVICE_UNAVAILABLE"] {
        let script = Arc::new(Script::default());
        script.data("EliteaBoard(", board_data());
        script.on_graph(
            "EliteaDraft",
            Reply::Json(200, json!({"errors": [{"type": kind, "message": "boom"}]})),
        );
        let says = rig(&script)
            .await
            .says("create_issue_on_project", args.clone())
            .await;
        assert!(
            says.contains("may or may not have been made"),
            "{kind}: {says}"
        );
        assert!(!says.contains("Nothing was sent"), "{says}");
    }
}

#[tokio::test]
async fn a_lost_conversion_answer_may_have_applied_and_only_a_refusal_is_not_converted() {
    let args = json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign",
                      "title": "t", "body": "b"});
    let make = |reply: Reply| {
        let script = Arc::new(Script::default());
        script.data("EliteaBoard(", board_data());
        script.data(
            "EliteaDraft",
            json!({"addProjectV2DraftIssue": {"projectItem": {"id": "DRAFT"}}}),
        );
        script.on_graph("EliteaConvert", reply);
        script
    };
    for reply in [
        Reply::Transport(TransportError::timeout()),
        Reply::Json(502, json!({})),
        Reply::Json(
            200,
            json!({"errors": [{"type": "INTERNAL", "message": "boom"}]}),
        ),
    ] {
        let says = rig(&make(reply))
            .await
            .says("create_issue_on_project", args.clone())
            .await;
        assert!(says.contains("may or may not have been applied"), "{says}");
        assert!(says.contains("check the board"), "{says}");
        assert!(!says.contains("was not converted"), "{says}");
    }
    let says = rig(&make(Reply::Json(
        200,
        json!({"errors": [{"type": "FORBIDDEN", "message": "no access"}]}),
    )))
    .await
    .says("create_issue_on_project", args)
    .await;
    assert!(says.contains("was not converted"), "{says}");
}

/// One GitHub HTTP client for everything, built without protocol retries: a
/// retried POST or PUT is a second effect and hides a lost answer.
#[test]
fn the_github_client_is_built_without_protocol_retries() {
    use elitea_connectors::github::client::CLIENT_POLICY;
    let retries_disabled = std::hint::black_box(CLIENT_POLICY.disable_retries);
    assert!(retries_disabled);
}

#[tokio::test]
async fn string_numbers_are_read_by_the_project_reads_too() {
    let script = Arc::new(Script::default());
    script.data(
        "EliteaProjectSearch",
        json!({"repository": {"projectV2": {"id": "P", "title": "B", "url": "u",
            "fields": {"nodes": []},
            "items": {"totalCount": 0, "pageInfo": {"hasNextPage": false}, "nodes": []}}}}),
    );
    let rig = rig(&script).await;
    rig.call(
        "search_project_issues",
        json!({"board_repo": "EliteaAI/board", "project_number": "4", "search_query": "q", "items_count": "5"}),
    )
    .await;
    let hits = script.hits();
    assert_eq!(body_of(&hits[0])["variables"]["projectNumber"], 4);
    assert_eq!(body_of(&hits[0])["variables"]["itemsCount"], 5);
}

// ----- review round 2 ---------------------------------------------------------------------------------------

#[tokio::test]
async fn a_crlf_patch_applies_and_its_paths_carry_no_carriage_return() {
    let script = Arc::new(Script::default());
    script.json(
        "GET",
        "/contents/src/a.txt",
        200,
        file_json("one\r\ntwo\r\nthree\r\n", 'a'),
    );
    script.json("PUT", "/contents/src/a.txt", 200, json!({}));
    let rig = rig(&script).await;
    let patch = "diff --git a/src/a.txt b/src/a.txt\r\nindex 1..2 100644\r\n--- a/src/a.txt\r\n+++ b/src/a.txt\r\n@@ -1,3 +1,3 @@\r\n one\r\n-two\r\n+TWO\r\n three\r\n";
    let result = rig
        .call(
            "apply_git_patch",
            json!({"patch_content": patch, "branch": "work"}),
        )
        .await;
    assert_eq!(
        result["applied_changes"],
        json!(["Modified: src/a.txt"]),
        "{result}"
    );
    let writes = script.writes();
    assert_eq!(writes.len(), 1);
    assert!(
        writes[0].path.ends_with("/contents/src/a.txt"),
        "{}",
        writes[0].path
    );
    assert_eq!(
        body_of(&writes[0])["content"],
        STANDARD.encode("one\r\nTWO\r\nthree\r\n"),
        "the file keeps its CRLF line ends"
    );
}

#[tokio::test]
async fn a_patch_that_touches_one_path_twice_is_refused_whole() {
    let script = Arc::new(Script::default());
    let rig = rig(&script).await;
    let two = |a: &str, b: &str| format!("{a}{b}");
    let modify = "diff --git a/f.txt b/f.txt\n--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-a\n+b\n";
    let cases = [
        // The same file in two diffs.
        (two(modify, modify), "f.txt"),
        // A rename whose target is also modified.
        (
            two(
                "diff --git a/old.txt b/new.txt\nrename from old.txt\nrename to new.txt\n",
                "diff --git a/new.txt b/new.txt\n--- a/new.txt\n+++ b/new.txt\n@@ -1 +1 @@\n-a\n+b\n",
            ),
            "new.txt",
        ),
        // A delete and a create of one path.
        (
            two(
                "diff --git a/g.txt b/g.txt\ndeleted file mode 100644\n--- a/g.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n",
                "diff --git a/g.txt b/g.txt\nnew file mode 100644\n--- /dev/null\n+++ b/g.txt\n@@ -0,0 +1 @@\n+a\n",
            ),
            "g.txt",
        ),
        // A rename's source modified too.
        (
            two(
                "diff --git a/s.txt b/t.txt\nrename from s.txt\nrename to t.txt\n",
                "diff --git a/s.txt b/s.txt\n--- a/s.txt\n+++ b/s.txt\n@@ -1 +1 @@\n-a\n+b\n",
            ),
            "s.txt",
        ),
    ];
    for (patch, path) in cases {
        let says = rig
            .says(
                "apply_git_patch",
                json!({"patch_content": patch, "branch": "work"}),
            )
            .await;
        assert_eq!(
            says,
            format!(
                "the patch changes `{path}` more than once; split it into separate patches, one per change to that file"
            )
        );
    }
    assert!(script.hits().is_empty(), "nothing was read or written");
}

#[tokio::test]
async fn a_full_ref_is_refused_where_a_branch_name_is_expected() {
    let script = Arc::new(Script::default());
    let rig = rig(&script).await;
    for (tool, arguments) in [
        ("delete_branch", json!({"branch_name": "refs/heads/main"})),
        (
            "set_active_branch",
            json!({"branch_name": "refs/heads/main"}),
        ),
        (
            "create_branch",
            json!({"proposed_branch_name": "refs/heads/x"}),
        ),
        (
            "create_file",
            json!({"file_path": "a.txt", "branch": "refs/heads/main", "file_contents": "x"}),
        ),
        (
            "create_pull_request",
            json!({"title": "t", "body": "b", "head": "refs/heads/work"}),
        ),
    ] {
        let says = rig.says(tool, arguments).await;
        assert!(
            says.contains("must be a branch name such as `main`, not a full ref"),
            "{tool}: {says}"
        );
    }
    assert!(script.hits().is_empty(), "refs/heads/main deleted nothing");
    // A branch that merely starts with "refs" is a branch.
    script.json("DELETE", "/git/refs/heads/refs-fix", 204, json!(null));
    assert_eq!(
        rig.says("delete_branch", json!({"branch_name": "refs-fix"}))
            .await,
        "Branch 'refs-fix' deleted successfully."
    );
}

/// An error that is not clearly decided before the mutation ran (untyped,
/// timed out, or beside partial data) may follow a mutation that applied.
#[tokio::test]
async fn a_mutation_error_is_definite_only_when_it_is_typed_and_carries_no_data() {
    let args = json!({"board_repo": "EliteaAI/board", "project_title": "WebApp Redesign",
                      "title": "t", "body": "b"});
    let draft_with = |body: Value| {
        let script = Arc::new(Script::default());
        script.data("EliteaBoard(", board_data());
        script.on_graph("EliteaDraft", Reply::Json(200, body));
        script
    };
    for body in [
        json!({"errors": [{"message": "Something timed out, please try again"}]}),
        json!({"errors": [{"message": "Timeout on validation of query"}]}),
        json!({"errors": [{"type": "SOMETHING_NEW", "message": "x"}]}),
        json!({"errors": [{"type": "NOT_FOUND", "message": "x"}, {"message": "untyped"}]}),
        json!({"data": {"addProjectV2DraftIssue": {"projectItem": {"id": "D"}}},
               "errors": [{"type": "FORBIDDEN", "message": "one field was hidden"}]}),
    ] {
        let says = rig(&draft_with(body.clone()))
            .await
            .says("create_issue_on_project", args.clone())
            .await;
        assert!(
            says.contains("may or may not have been made"),
            "{body}: {says}"
        );
    }
    for (kind, expected) in [
        ("NOT_FOUND", "did not find"),
        ("UNPROCESSABLE", "Draft Issue Not Created"),
        ("FORBIDDEN", "Draft Issue Not Created"),
    ] {
        let says = rig(&draft_with(
            json!({"data": null, "errors": [{"type": kind, "message": "decided"}]}),
        ))
        .await
        .says("create_issue_on_project", args.clone())
        .await;
        assert!(!says.contains("may or may not"), "{kind}: {says}");
        assert!(
            says.contains(expected) || says.contains("Draft Issue Not Created"),
            "{kind}: {says}"
        );
    }
}

// ----- ADR-0034: create_branch from a commit, and open_pull_request --------------------------------

#[tokio::test]
async fn create_branch_from_sha_branches_from_that_commit_without_reading_a_branch() {
    let script = Arc::new(Script::default());
    script.json("POST", "/git/refs", 201, json!({"ref": "refs/heads/work"}));
    let rig = rig(&script).await;
    let says = rig
        .says(
            "create_branch",
            json!({"proposed_branch_name": "work", "from_sha": sha('d')}),
        )
        .await;
    assert!(says.contains("Branch 'work' created"), "{says}");
    let hits = script.hits();
    assert_eq!(hits.len(), 1, "no branch read: {hits:?}");
    assert_eq!(
        body_of(&hits[0]),
        &json!({"ref": "refs/heads/work", "sha": sha('d')})
    );
    // A malformed commit id is refused before anything is sent.
    let script = Arc::new(Script::default());
    let rig = super::github_writes_tests::rig(&script).await;
    let _ = rig
        .call(
            "create_branch",
            json!({"proposed_branch_name": "work", "from_sha": "abc"}),
        )
        .await;
    assert!(script.hits().is_empty());
}

mod open_pull_request {
    use super::*;
    use crate::toolkits::workspace_pull_request::{
        CredentialUnavailable, DiffUnavailable, ForgeKind, PullRequestAudit, PullRequestOpened,
        PullRequestTarget, THREAD_CREDENTIAL, ThreadConfiguration, ThreadCredential,
        ThreadCredentialSource, WorkspaceDiff, WorkspaceDiffSource, open_pull_request_toolset,
    };
    use zeroize::Zeroizing;

    struct Source(Result<WorkspaceDiff, DiffUnavailable>);

    #[async_trait]
    impl WorkspaceDiffSource for Source {
        async fn pull_request_diff(&self) -> Result<WorkspaceDiff, DiffUnavailable> {
            self.0.clone()
        }
    }

    #[derive(Default)]
    struct Audit(Mutex<Vec<PullRequestOpened>>);

    impl PullRequestAudit for Audit {
        fn opened(&self, event: &PullRequestOpened) {
            self.0.lock().expect("audit").push(event.clone());
        }
    }

    fn diff(text: &str) -> WorkspaceDiff {
        WorkspaceDiff {
            upstream_sha: sha('d'),
            upstream_branch: "main".to_owned(),
            remote_url: "https://github.example.test/EliteaAI/elitea-platform.git".to_owned(),
            text: text.to_owned(),
        }
    }

    async fn open(
        script: &Arc<Script>,
        source: Result<WorkspaceDiff, DiffUnavailable>,
        audit: &Arc<Audit>,
    ) -> Value {
        let toolset = open_pull_request_toolset(
            None,
            vec![PullRequestTarget::github_test(
                "team-github",
                &settings(&[]),
                &(script.clone() as Arc<dyn Transport>),
            )],
            Arc::new(Source(source)),
            audit.clone(),
        )
        .expect("a bound GitHub toolkit offers the tool");
        let readonly: Arc<dyn ReadonlyContext> = context();
        let tools = adk_core::Toolset::tools(toolset.as_ref(), readonly)
            .await
            .expect("tools");
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0].name(), "open_pull_request");
        let schema = tools[0].parameters_schema().expect("schema").to_string();
        assert!(
            !schema.contains("patch") && !schema.contains("diff\""),
            "the diff is never an argument"
        );
        let ctx: Arc<dyn ToolContext> = context();
        tools[0]
            .execute(ctx, json!({"title": "Cloud change", "body": "From the cloud", "branch": "cloud-change"}))
            .await
            .expect("result")
    }

    #[tokio::test]
    async fn it_branches_from_upstream_applies_the_diff_and_opens_against_the_upstream_branch() {
        let script = Arc::new(Script::default());
        // The name is taken: the tool uses the `_vN` name GitHub accepted.
        script.json(
            "POST",
            "/git/refs",
            422,
            json!({"message": "Reference already exists"}),
        );
        script.json(
            "GET",
            "/git/matching-refs/heads/cloud-change_v",
            200,
            json!([]),
        );
        script.json("POST", "/git/refs", 201, json!({}));
        script.json(
            "GET",
            "/contents/src/a.txt",
            200,
            file_json("one\ntwo\n", 'a'),
        );
        script.json("GET", "/contents/src/new.txt", 404, json!({}));
        script.json("PUT", "/contents/src/a.txt", 200, json!({}));
        script.json("PUT", "/contents/src/new.txt", 201, json!({}));
        script.json("POST", "/pulls", 201, json!({"number": 7, "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/7"}));
        let audit = Arc::new(Audit::default());
        let result = open(&script, Ok(diff(PATCH_TWO_FILES)), &audit).await;
        assert_eq!(result["status"], "ok", "{result}");
        assert_eq!(result["branch"], "cloud-change_v1");
        let writes = script.writes();
        assert_eq!(
            body_of(&writes[0])["sha"],
            sha('d'),
            "from the upstream commit"
        );
        let pull = writes
            .iter()
            .find(|hit| hit.path.ends_with("/pulls"))
            .expect("a pull request");
        assert_eq!(
            body_of(pull),
            &json!({"title": "Cloud change", "body": "From the cloud", "head": "cloud-change_v1", "base": "main"})
        );
        let events = audit.0.lock().expect("audit");
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].number, 7);
        assert_eq!(
            events[0].url,
            "https://github.example.test/EliteaAI/elitea-platform/pull/7"
        );
        assert_eq!(events[0].files, 2);
    }

    #[tokio::test]
    async fn a_write_that_fails_partway_reports_the_branch_and_opens_nothing() {
        let script = Arc::new(Script::default());
        script.json("POST", "/git/refs", 201, json!({}));
        script.json(
            "GET",
            "/contents/src/a.txt",
            200,
            file_json("one\ntwo\n", 'a'),
        );
        script.json("GET", "/contents/src/new.txt", 404, json!({}));
        script.json("PUT", "/contents/src/a.txt", 200, json!({}));
        script.json(
            "PUT",
            "/contents/src/new.txt",
            403,
            json!({"message": "Resource not accessible"}),
        );
        let audit = Arc::new(Audit::default());
        let result = open(&script, Ok(diff(PATCH_TWO_FILES)), &audit).await;
        assert_eq!(result["status"], "partial", "{result}");
        assert_eq!(result["branch"], "cloud-change");
        assert_eq!(result["applied_changes"], json!(["Modified: src/a.txt"]));
        assert!(
            script
                .writes()
                .iter()
                .all(|hit| !hit.path.ends_with("/pulls"))
        );
        assert!(audit.0.lock().expect("audit").is_empty());
    }

    #[tokio::test]
    async fn a_binary_change_is_refused_before_any_write() {
        let script = Arc::new(Script::default());
        let binary = "diff --git a/logo.png b/logo.png\nnew file mode 100644\nBinary files /dev/null and b/logo.png differ\n";
        let result = open(&script, Ok(diff(binary)), &Arc::new(Audit::default())).await;
        assert_eq!(result["status"], "refused", "{result}");
        assert!(script.hits().is_empty(), "nothing was sent");
        let result = open(
            &script,
            Err(DiffUnavailable::Binary),
            &Arc::new(Audit::default()),
        )
        .await;
        assert_eq!(result["status"], "refused");
        assert!(script.hits().is_empty());
    }

    #[tokio::test]
    async fn no_upstream_or_another_repository_is_refused_and_no_toolkit_means_no_tool() {
        let script = Arc::new(Script::default());
        let result = open(
            &script,
            Err(DiffUnavailable::NoUpstream),
            &Arc::new(Audit::default()),
        )
        .await;
        assert_eq!(result["status"], "refused");
        assert!(
            result["message"]
                .as_str()
                .expect("message")
                .contains("no upstream")
        );
        let mut other = diff(PATCH_TWO_FILES);
        other.remote_url = "git@github.example.test:someone/else.git".to_owned();
        let result = open(&script, Ok(other), &Arc::new(Audit::default())).await;
        assert_eq!(result["status"], "refused");
        assert!(script.hits().is_empty());
        assert!(
            open_pull_request_toolset(
                None,
                Vec::new(),
                Arc::new(Source(Ok(diff("")))),
                Arc::new(Audit::default())
            )
            .is_none(),
            "without a bound GitHub toolkit the tool is absent"
        );
    }

    /// The thread's credential: the redeemed configuration's data, counted.
    struct Credential {
        answer: Result<String, CredentialUnavailable>,
        calls: Mutex<usize>,
    }

    impl Credential {
        fn new(answer: Result<String, CredentialUnavailable>) -> Arc<Self> {
            Arc::new(Self {
                answer,
                calls: Mutex::new(0),
            })
        }

        fn calls(&self) -> usize {
            *self.calls.lock().expect("calls")
        }
    }

    #[async_trait]
    impl ThreadCredentialSource for Credential {
        async fn configuration(&self) -> Result<ThreadConfiguration, CredentialUnavailable> {
            *self.calls.lock().expect("calls") += 1;
            self.answer.clone().map(|json| ThreadConfiguration {
                forge: ForgeKind::GitHub,
                json: Zeroizing::new(json),
            })
        }
    }

    fn configuration(base_url: &str) -> String {
        json!({"base_url": base_url, "access_token": "thread-token"}).to_string()
    }

    /// Open through `@thread` only (no toolkit bound), over the fake GitHub
    /// API in `script`.
    async fn open_thread(
        script: &Arc<Script>,
        credential: &Arc<Credential>,
        source: Result<WorkspaceDiff, DiffUnavailable>,
        audit: &Arc<Audit>,
        args: Value,
    ) -> Value {
        let thread = ThreadCredential::test(credential.clone(), script.clone());
        let toolset = open_pull_request_toolset(
            Some(thread),
            Vec::new(),
            Arc::new(Source(source)),
            audit.clone(),
        )
        .expect("a thread credential offers the tool");
        let readonly: Arc<dyn ReadonlyContext> = context();
        let tools = adk_core::Toolset::tools(toolset.as_ref(), readonly)
            .await
            .expect("tools");
        let schema = tools[0].parameters_schema().expect("schema");
        assert_eq!(
            schema["properties"]["toolkit"]["enum"],
            json!([THREAD_CREDENTIAL])
        );
        let ctx: Arc<dyn ToolContext> = context();
        tools[0].execute(ctx, args).await.expect("result")
    }

    fn change() -> Value {
        json!({"title": "Cloud change", "body": "From the cloud", "branch": "cloud-change"})
    }

    #[tokio::test]
    async fn the_thread_credential_opens_on_the_upstream_repository_without_a_toolkit() {
        let script = Arc::new(Script::default());
        script.json("POST", "/git/refs", 201, json!({}));
        script.json(
            "GET",
            "/contents/src/a.txt",
            200,
            file_json("one\ntwo\n", 'a'),
        );
        script.json("GET", "/contents/src/new.txt", 404, json!({}));
        script.json("PUT", "/contents/src/a.txt", 200, json!({}));
        script.json("PUT", "/contents/src/new.txt", 201, json!({}));
        script.json("POST", "/pulls", 201, json!({"number": 9, "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/9"}));
        let credential = Credential::new(Ok(configuration("https://github.example.test/api/v3")));
        let audit = Arc::new(Audit::default());
        let result = open_thread(
            &script,
            &credential,
            Ok(diff(PATCH_TWO_FILES)),
            &audit,
            change(),
        )
        .await;
        assert_eq!(result["status"], "ok", "{result}");
        assert_eq!(
            result["url"],
            "https://github.example.test/EliteaAI/elitea-platform/pull/9"
        );
        assert_eq!(credential.calls(), 1, "redeemed once, when the tool ran");
        let hits = script.hits();
        assert!(
            hits.iter()
                .all(|hit| hit.path.contains("/repos/EliteaAI/elitea-platform/")),
            "every call names the upstream's repository: {hits:?}"
        );
        let pull = script
            .writes()
            .into_iter()
            .find(|hit| hit.path.ends_with("/pulls"))
            .expect("a pull request");
        assert_eq!(body_of(&pull)["base"], "main");
        let events = audit.0.lock().expect("audit");
        assert_eq!(events[0].toolkit, THREAD_CREDENTIAL);
        assert_eq!(events[0].repository, "EliteaAI/elitea-platform");
        let recorded = format!("{:?}{result}", events[0]);
        assert!(
            !recorded.contains("thread-token"),
            "no token in the audit or the answer"
        );
    }

    #[tokio::test]
    async fn the_thread_credential_is_refused_for_another_host_unset_or_disabled() {
        let script = Arc::new(Script::default());
        let audit = Arc::new(Audit::default());
        let other_host = Credential::new(Ok(configuration("https://api.github.com")));
        let result = open_thread(
            &script,
            &other_host,
            Ok(diff(PATCH_TWO_FILES)),
            &audit,
            change(),
        )
        .await;
        assert_eq!(result["status"], "refused", "{result}");
        assert!(
            result["message"]
                .as_str()
                .expect("message")
                .contains("github.com")
        );
        for unavailable in [
            CredentialUnavailable::NotChosen,
            CredentialUnavailable::Disabled,
            CredentialUnavailable::Invalid,
            CredentialUnavailable::Failed,
        ] {
            let credential = Credential::new(Err(unavailable));
            let result = open_thread(
                &script,
                &credential,
                Ok(diff(PATCH_TWO_FILES)),
                &audit,
                change(),
            )
            .await;
            assert_eq!(result["status"], "refused", "{unavailable:?}: {result}");
        }
        let broken = Credential::new(Ok(
            "{\"base_url\": \"http://plain.example.test\"}".to_owned()
        ));
        let result = open_thread(
            &script,
            &broken,
            Ok(diff(PATCH_TWO_FILES)),
            &audit,
            change(),
        )
        .await;
        assert_eq!(result["status"], "refused", "{result}");
        assert!(script.hits().is_empty(), "nothing reached GitHub");
        assert!(audit.0.lock().expect("audit").is_empty());
    }

    #[tokio::test]
    async fn a_branch_named_as_the_upstream_is_refused_before_the_credential_is_redeemed() {
        let script = Arc::new(Script::default());
        let credential = Credential::new(Ok(configuration("https://github.example.test/api/v3")));
        for branch in ["main", "refs/heads/main", "MAIN"] {
            let result = open_thread(
                &script,
                &credential,
                Ok(diff(PATCH_TWO_FILES)),
                &Arc::new(Audit::default()),
                json!({"title": "t", "branch": branch}),
            )
            .await;
            assert_eq!(result["status"], "refused", "{branch}: {result}");
        }
        assert_eq!(credential.calls(), 0);
        assert!(script.hits().is_empty());
    }

    #[tokio::test]
    async fn an_ssh_alias_remote_is_scoped_by_its_repository() {
        let script = Arc::new(Script::default());
        script.json("POST", "/git/refs", 201, json!({}));
        script.json(
            "GET",
            "/contents/src/a.txt",
            200,
            file_json("one\ntwo\n", 'a'),
        );
        script.json("GET", "/contents/src/new.txt", 404, json!({}));
        script.json("PUT", "/contents/src/a.txt", 200, json!({}));
        script.json("PUT", "/contents/src/new.txt", 201, json!({}));
        script.json(
            "POST",
            "/pulls",
            201,
            json!({"number": 3, "html_url": "https://github.example.test/o/r/pull/3"}),
        );
        let mut aliased = diff(PATCH_TWO_FILES);
        aliased.remote_url = "github-work:EliteaAI/elitea-platform.git".to_owned();
        let credential = Credential::new(Ok(configuration("https://github.example.test/api/v3")));
        let result = open_thread(
            &script,
            &credential,
            Ok(aliased),
            &Arc::new(Audit::default()),
            change(),
        )
        .await;
        assert_eq!(result["status"], "ok", "{result}");
    }
}

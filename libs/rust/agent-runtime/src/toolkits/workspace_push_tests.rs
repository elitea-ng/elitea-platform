//! `git_push` and `open_pull_request` over a real git push (ADR-0034
//! decision 10, amendment 2026-10-26), against one fake forge: a git
//! smart-HTTP `receive-pack` endpoint and the GitHub and GitLab APIs. The
//! real receive-pack client, GitHub client and GitLab client run; only the
//! network is replaced.

use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use elitea_connectors::transport::header::{self, HeaderValue};
use elitea_connectors::transport::{
    HeaderMap, Request, Response, StatusCode, Transport, TransportError,
};
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::workspace_pull_request::{
    BranchPushed, CredentialUnavailable, DiffUnavailable, ForgeKind, PreparedPush,
    PullRequestAudit, PullRequestOpened, PullRequestTarget, PushPack, PushUnavailable,
    THREAD_CREDENTIAL, ThreadConfiguration, ThreadCredential, ThreadCredentialSource, Upstream,
    WorkspaceDiff, WorkspaceDiffSource, WorkspaceForgeTools, WorkspacePushSource,
};

const TIP: &str = "1111111111111111111111111111111111111111";
const UPSTREAM: &str = "dddddddddddddddddddddddddddddddddddddddd";

#[derive(Clone, Debug)]
struct Hit {
    method: String,
    url: String,
    authorization: Option<String>,
    authorization_sensitive: bool,
    private_token: Option<String>,
    body: Vec<u8>,
}

/// One fake forge. A request it has no answer for fails the test.
#[derive(Default)]
struct Forge {
    /// Branches the git endpoint advertises.
    branches: Mutex<Vec<String>>,
    /// What `receive-pack` answers (default: accepted).
    report: Mutex<Option<Vec<u8>>>,
    /// The status of every git request (default 200).
    git_status: Mutex<Option<u16>>,
    hits: Mutex<Vec<Hit>>,
}

fn pkt(out: &mut Vec<u8>, line: &str) {
    out.extend_from_slice(format!("{:04x}", line.len() + 4).as_bytes());
    out.extend_from_slice(line.as_bytes());
}

impl Forge {
    fn with_branches(branches: &[&str]) -> Arc<Self> {
        let forge = Self::default();
        *forge.branches.lock().expect("branches") =
            branches.iter().map(|name| (*name).to_owned()).collect();
        Arc::new(forge)
    }

    fn hits(&self) -> Vec<Hit> {
        self.hits.lock().expect("hits").clone()
    }

    fn advertisement(&self) -> Vec<u8> {
        let mut out = Vec::new();
        pkt(&mut out, "# service=git-receive-pack\n");
        out.extend_from_slice(b"0000");
        let branches = self.branches.lock().expect("branches").clone();
        let mut names: Vec<String> = branches
            .iter()
            .map(|name| format!("refs/heads/{name}"))
            .collect();
        if names.is_empty() {
            names.push("capabilities^{}".to_owned());
        }
        for (index, name) in names.iter().enumerate() {
            let id = if name.starts_with("capabilities") {
                "0".repeat(40)
            } else {
                "a".repeat(40)
            };
            if index == 0 {
                pkt(
                    &mut out,
                    &format!("{id} {name}\0report-status delete-refs side-band-64k agent=fake\n"),
                );
            } else {
                pkt(&mut out, &format!("{id} {name}\n"));
            }
        }
        out.extend_from_slice(b"0000");
        out
    }

    fn receive(&self, body: &[u8]) -> Vec<u8> {
        if let Some(report) = self.report.lock().expect("report").clone() {
            return report;
        }
        // `<old> <new> <ref>\0caps\n`: the first (only) command.
        let length = usize::from_str_radix(std::str::from_utf8(&body[..4]).expect("hex"), 16)
            .expect("a length");
        let command = String::from_utf8_lossy(&body[4..length]).into_owned();
        let reference = command
            .split('\0')
            .next()
            .and_then(|line| line.split(' ').nth(2))
            .expect("a ref")
            .to_owned();
        let mut out = Vec::new();
        pkt(&mut out, "unpack ok\n");
        pkt(&mut out, &format!("ok {reference}\n"));
        out.extend_from_slice(b"0000");
        out
    }
}

fn reply(status: u16, content_type: &'static str, body: Vec<u8>) -> Response {
    let mut headers = HeaderMap::new();
    headers.insert(header::CONTENT_TYPE, HeaderValue::from_static(content_type));
    Response::from_bytes(StatusCode::from_u16(status).expect("status"), headers, body)
}

#[async_trait]
impl Transport for Forge {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        let url = request.url().to_string();
        let path = request.url().path().to_owned();
        let body = request
            .body()
            .and_then(|body| body.as_bytes())
            .map(<[u8]>::to_vec)
            .unwrap_or_default();
        let authorization = request.headers().get(header::AUTHORIZATION);
        self.hits.lock().expect("hits").push(Hit {
            method: request.method().to_string(),
            url: url.clone(),
            authorization: authorization
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
            authorization_sensitive: authorization.is_some_and(HeaderValue::is_sensitive),
            private_token: request
                .headers()
                .get("private-token")
                .and_then(|value| value.to_str().ok())
                .map(str::to_owned),
            body: body.clone(),
        });
        let git_status = self.git_status.lock().expect("status").unwrap_or(200);
        if path.ends_with(".git/info/refs") {
            return Ok(reply(
                git_status,
                "application/x-git-receive-pack-advertisement",
                self.advertisement(),
            ));
        }
        if path.ends_with(".git/git-receive-pack") {
            return Ok(reply(
                git_status,
                "application/x-git-receive-pack-result",
                self.receive(&body),
            ));
        }
        if path.ends_with("/pulls") {
            return Ok(reply(
                201,
                "application/json",
                json!({"number": 12, "html_url": "https://github.example.test/EliteaAI/elitea-platform/pull/12"})
                    .to_string()
                    .into_bytes(),
            ));
        }
        if path.ends_with("/merge_requests") {
            return Ok(reply(
                201,
                "application/json",
                json!({"iid": 4, "web_url": "https://gitlab.example.test/Group/Sub/repo/-/merge_requests/4"})
                    .to_string()
                    .into_bytes(),
            ));
        }
        panic!(
            "the fake forge has no answer for {} {url}",
            request.method()
        );
    }
}

struct Source {
    upstream: Upstream,
    answer: Option<PushUnavailable>,
    calls: Mutex<usize>,
}

#[async_trait]
impl WorkspacePushSource for Source {
    async fn upstream(&self) -> Result<Upstream, PushUnavailable> {
        match self.answer {
            Some(PushUnavailable::NoUpstream) => Err(PushUnavailable::NoUpstream),
            _ => Ok(self.upstream.clone()),
        }
    }

    async fn prepare_push(&self) -> Result<PreparedPush, PushUnavailable> {
        *self.calls.lock().expect("calls") += 1;
        if let Some(unavailable) = self.answer {
            return Err(unavailable);
        }
        Ok(PreparedPush {
            upstream: self.upstream.clone(),
            tip: TIP.to_owned(),
            commits: 3,
            files: 2,
            pack: PushPack::from_pack(b"PACK\0\0\0\x02\0\0\0\0fake-pack"),
        })
    }
}

struct NoDiff;

#[async_trait]
impl WorkspaceDiffSource for NoDiff {
    async fn pull_request_diff(&self) -> Result<WorkspaceDiff, DiffUnavailable> {
        Err(DiffUnavailable::Empty)
    }
}

#[derive(Default)]
struct Audit {
    opened: Mutex<Vec<PullRequestOpened>>,
    pushed: Mutex<Vec<BranchPushed>>,
}

impl PullRequestAudit for Audit {
    fn opened(&self, event: &PullRequestOpened) {
        self.opened.lock().expect("audit").push(event.clone());
    }

    fn pushed(&self, event: &BranchPushed) {
        self.pushed.lock().expect("audit").push(event.clone());
    }
}

struct Credential {
    forge: ForgeKind,
    /// What Main answers now (a test may change it between calls).
    json: Mutex<Result<String, CredentialUnavailable>>,
    calls: Mutex<usize>,
}

impl Credential {
    fn calls(&self) -> usize {
        *self.calls.lock().expect("calls")
    }

    fn answer(&self, answer: Result<String, CredentialUnavailable>) {
        *self.json.lock().expect("json") = answer;
    }
}

#[async_trait]
impl ThreadCredentialSource for Credential {
    async fn configuration(&self) -> Result<ThreadConfiguration, CredentialUnavailable> {
        *self.calls.lock().expect("calls") += 1;
        self.json
            .lock()
            .expect("json")
            .clone()
            .map(|json| ThreadConfiguration {
                forge: self.forge,
                json: Zeroizing::new(json),
            })
    }
}

fn github_credential() -> Arc<Credential> {
    Arc::new(Credential {
        forge: ForgeKind::GitHub,
        json: Mutex::new(Ok(json!({"base_url": "https://github.example.test/api/v3", "access_token": "ghp-thread-token"})
            .to_string())),
        calls: Mutex::new(0),
    })
}

fn gitlab_credential(url: &str) -> Arc<Credential> {
    Arc::new(Credential {
        forge: ForgeKind::GitLab,
        json: Mutex::new(Ok(
            json!({"url": url, "private_token": "glpat-thread-token"}).to_string(),
        )),
        calls: Mutex::new(0),
    })
}

fn upstream(remote: &str) -> Upstream {
    Upstream {
        sha: UPSTREAM.to_owned(),
        branch: "main".to_owned(),
        remote_url: remote.to_owned(),
    }
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("workspace-push-test")
            .with_session_id("session-1")
            .with_function_call_id("call-1"),
    )
}

struct Rig {
    tools: Vec<Arc<dyn Tool>>,
    audit: Arc<Audit>,
    source: Arc<Source>,
}

impl Rig {
    async fn new(
        forge: &Arc<Forge>,
        thread: Option<Arc<Credential>>,
        targets: Vec<PullRequestTarget>,
        source: Source,
    ) -> Self {
        let audit = Arc::new(Audit::default());
        let source = Arc::new(source);
        let transport: Arc<dyn Transport> = forge.clone();
        let toolset = WorkspaceForgeTools {
            thread: thread
                .map(|credential| ThreadCredential::test(credential, Arc::clone(&transport))),
            targets,
            diff: Arc::new(NoDiff),
            push: Some(source.clone()),
            push_transport: Some(transport),
            audit: audit.clone(),
        }
        .toolset()
        .expect("a credential offers the tools");
        let readonly: Arc<dyn ReadonlyContext> = context();
        let tools = adk_core::Toolset::tools(toolset.as_ref(), readonly)
            .await
            .expect("tools");
        Self {
            tools,
            audit,
            source,
        }
    }

    async fn call(&self, name: &str, args: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|tool| tool.name() == name)
            .expect("the tool");
        let ctx: Arc<dyn ToolContext> = context();
        tool.execute(ctx, args).await.expect("a result")
    }
}

fn source(remote: &str) -> Source {
    Source {
        upstream: upstream(remote),
        answer: None,
        calls: Mutex::new(0),
    }
}

/// The push command the fake received: `<old> <new> <ref>`.
fn command(hit: &Hit) -> String {
    let length = usize::from_str_radix(std::str::from_utf8(&hit.body[..4]).expect("hex"), 16)
        .expect("a length");
    String::from_utf8_lossy(&hit.body[4..length])
        .split('\0')
        .next()
        .expect("a command")
        .to_owned()
}

fn basic(user: &str, secret: &str) -> String {
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("{user}:{secret}"))
    )
}

#[tokio::test]
async fn git_push_creates_a_new_branch_on_the_upstream_repository_and_never_moves_one() {
    let forge = Forge::with_branches(&["main", "cloud-change"]);
    let credential = github_credential();
    let rig = Rig::new(
        &forge,
        Some(credential.clone()),
        Vec::new(),
        // An ssh remote is pushed to over HTTPS on the credential's host.
        source("git@github.example.test:EliteaAI/elitea-platform.git"),
    )
    .await;
    let names: Vec<&str> = rig.tools.iter().map(|tool| tool.name()).collect();
    assert_eq!(names, ["open_pull_request", "git_push"]);
    let result = rig
        .call("git_push", json!({"branch": "cloud-change"}))
        .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(
        result["branch"], "cloud-change_v2",
        "a taken name gets a suffix"
    );
    assert_eq!(
        result["url"],
        "https://github.example.test/EliteaAI/elitea-platform/tree/cloud-change_v2"
    );
    let hits = forge.hits();
    assert_eq!(hits.len(), 2, "{hits:?}");
    assert_eq!(
        hits[0].url,
        "https://github.example.test/EliteaAI/elitea-platform.git/info/refs?service=git-receive-pack"
    );
    assert_eq!(hits[1].method, "POST");
    assert_eq!(
        command(&hits[1]),
        format!("{} {TIP} refs/heads/cloud-change_v2", "0".repeat(40)),
        "create-only: the old id is zero"
    );
    assert!(hits[1].body.windows(4).any(|window| window == b"PACK"));
    for hit in &hits {
        assert_eq!(
            hit.authorization.as_deref(),
            Some(basic("x-access-token", "ghp-thread-token").as_str())
        );
        assert!(hit.authorization_sensitive);
    }
    let pushed = rig.audit.pushed.lock().expect("audit").clone();
    assert_eq!(pushed.len(), 1);
    assert_eq!(pushed[0].toolkit, THREAD_CREDENTIAL);
    assert_eq!(pushed[0].branch, "cloud-change_v2");
    assert_eq!(pushed[0].base, "main");
    assert_eq!(pushed[0].commits, 3);
    let recorded = format!("{pushed:?}{result}");
    assert!(
        !recorded.contains("ghp-thread-token"),
        "no token in the audit or the answer"
    );
    assert_eq!(credential.calls(), 1, "one redemption for the push");

    // The pull request opens from the pushed branch without a second push.
    let result = rig
        .call(
            "open_pull_request",
            json!({"title": "Cloud change", "body": "From the cloud", "branch": "cloud-change_v2", "push": false}),
        )
        .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["pull_request"], 12);
    let hits = forge.hits();
    assert_eq!(hits.len(), 3);
    assert!(
        hits[2]
            .url
            .ends_with("/api/v3/repos/EliteaAI/elitea-platform/pulls")
    );
    let pull: Value = serde_json::from_slice(&hits[2].body).expect("json");
    assert_eq!(
        pull,
        json!({"title": "Cloud change", "body": "From the cloud", "head": "cloud-change_v2", "base": "main"})
    );
    assert_eq!(*rig.source.calls.lock().expect("calls"), 1);
    assert_eq!(
        credential.calls(),
        2,
        "push: false redeems the credential again; nothing secret was kept"
    );
    let opened = rig.audit.opened.lock().expect("audit").clone();
    assert_eq!(opened.len(), 1);
    assert_eq!(opened[0].files, 2);

    // The project turned pull requests off after the push: the next call
    // is refused by Main's redemption, and nothing reaches the forge.
    credential.answer(Err(CredentialUnavailable::Disabled));
    let result = rig
        .call(
            "open_pull_request",
            json!({"title": "Again", "branch": "cloud-change_v2", "push": false}),
        )
        .await;
    assert_eq!(result["status"], "refused", "{result}");
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("turned off")
    );
    assert_eq!(forge.hits().len(), 3, "nothing after the refusal");
    assert_eq!(rig.audit.opened.lock().expect("audit").len(), 1);
}

#[tokio::test]
async fn an_ssh_alias_is_never_taken_for_the_credentials_host() {
    let forge = Forge::with_branches(&["main"]);
    let rig = Rig::new(
        &forge,
        Some(github_credential()),
        Vec::new(),
        source("github-work:EliteaAI/elitea-platform.git"),
    )
    .await;
    for (tool, args) in [
        ("git_push", json!({"branch": "x"})),
        ("open_pull_request", json!({"title": "t", "branch": "x"})),
    ] {
        let result = rig.call(tool, args).await;
        assert_eq!(result["status"], "refused", "{tool}: {result}");
        assert!(
            result["message"]
                .as_str()
                .expect("message")
                .contains("SSH alias `github-work`"),
            "{result}"
        );
    }
    assert!(forge.hits().is_empty(), "nothing was sent");
    assert_eq!(
        *rig.source.calls.lock().expect("calls"),
        0,
        "nothing was prepared"
    );
}

#[tokio::test]
async fn a_credential_that_cannot_push_goes_to_the_api_without_a_pack() {
    let forge = Forge::with_branches(&["main"]);
    let anonymous = Arc::new(Credential {
        forge: ForgeKind::GitHub,
        json: Mutex::new(Ok(
            json!({"base_url": "https://github.example.test/api/v3"}).to_string(),
        )),
        calls: Mutex::new(0),
    });
    let rig = Rig::new(
        &forge,
        Some(anonymous.clone()),
        Vec::new(),
        source("https://github.example.test/EliteaAI/elitea-platform.git"),
    )
    .await;
    let result = rig
        .call("open_pull_request", json!({"title": "t", "branch": "x"}))
        .await;
    // The fixture's diff source has no change: the API path's own answer.
    assert_eq!(result["status"], "refused", "{result}");
    assert_eq!(
        result["message"],
        "There is no change to open a pull request with."
    );
    assert_eq!(
        *rig.source.calls.lock().expect("calls"),
        0,
        "no pack is prepared for the API path"
    );
    assert_eq!(anonymous.calls(), 1, "one redemption for the call");
    let result = rig.call("git_push", json!({"branch": "x"})).await;
    assert_eq!(result["status"], "refused", "{result}");
    assert_eq!(*rig.source.calls.lock().expect("calls"), 0);
    assert!(forge.hits().is_empty());
}

#[tokio::test]
async fn a_gitlab_merge_request_pushes_then_opens_on_the_nested_project() {
    let forge = Forge::with_branches(&["main"]);
    let rig = Rig::new(
        &forge,
        Some(gitlab_credential("https://gitlab.example.test")),
        Vec::new(),
        source("ssh://git@gitlab.example.test:2222/Group/Sub/repo.git"),
    )
    .await;
    let result = rig
        .call(
            "open_pull_request",
            json!({"title": "Cloud change", "branch": "feature/cloud"}),
        )
        .await;
    assert_eq!(result["status"], "ok", "{result}");
    assert_eq!(result["forge"], "gitlab");
    assert!(result["message"].as_str().expect("message").contains("!4"));
    let hits = forge.hits();
    assert_eq!(hits.len(), 3, "{hits:?}");
    assert!(
        hits[0]
            .url
            .starts_with("https://gitlab.example.test/Group/Sub/repo.git/info/refs")
    );
    assert_eq!(
        hits[1].authorization.as_deref(),
        Some(basic("oauth2", "glpat-thread-token").as_str())
    );
    assert!(
        hits[2]
            .url
            .contains("/api/v4/projects/Group%2FSub%2Frepo/merge_requests")
    );
    assert_eq!(hits[2].private_token.as_deref(), Some("glpat-thread-token"));
    assert_eq!(
        hits[2].authorization, None,
        "the API takes the token header only"
    );
    let request: Value = serde_json::from_slice(&hits[2].body).expect("json");
    assert_eq!(request["source_branch"], "feature/cloud");
    assert_eq!(request["target_branch"], "main");
    let pushed = rig.audit.pushed.lock().expect("audit").clone();
    assert_eq!(pushed[0].forge, ForgeKind::GitLab);
    assert_eq!(
        pushed[0].url,
        "https://gitlab.example.test/Group/Sub/repo/-/tree/feature/cloud"
    );
    assert_eq!(rig.audit.opened.lock().expect("audit")[0].number, 4);
}

#[tokio::test]
async fn a_bound_gitlab_toolkit_pushes_only_to_its_own_repository() {
    let forge = Forge::with_branches(&["main"]);
    let settings: Map<String, Value> = json!({
        "gitlab_configuration": {"url": "https://gitlab.example.test", "private_token": "glpat-toolkit"},
        "repository": "group/repo",
        "branch": "main"
    })
    .as_object()
    .expect("settings")
    .clone();
    let transport: Arc<dyn Transport> = forge.clone();
    let rig = Rig::new(
        &forge,
        None,
        vec![PullRequestTarget::gitlab_test(
            "team-gitlab",
            &settings,
            &transport,
        )],
        source("https://gitlab.example.test/other/repo.git"),
    )
    .await;
    let result = rig.call("git_push", json!({"branch": "x"})).await;
    assert_eq!(result["status"], "refused", "{result}");
    assert!(forge.hits().is_empty(), "nothing was sent");
}

#[tokio::test]
async fn refusals_send_nothing_and_record_nothing() {
    let forge = Forge::with_branches(&["main"]);
    // Another host than the credential's.
    let rig = Rig::new(
        &forge,
        Some(gitlab_credential("https://gitlab.example.test")),
        Vec::new(),
        source("https://gitlab.com/Group/repo.git"),
    )
    .await;
    let result = rig.call("git_push", json!({"branch": "x"})).await;
    assert_eq!(result["status"], "refused", "{result}");
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("gitlab.com")
    );
    // The upstream branch, and names git refuses, before any credential.
    let credential = github_credential();
    let rig = Rig::new(
        &forge,
        Some(credential.clone()),
        Vec::new(),
        source("https://github.example.test/EliteaAI/elitea-platform.git"),
    )
    .await;
    for branch in ["main", "refs/heads/MAIN"] {
        let result = rig.call("git_push", json!({"branch": branch})).await;
        assert_eq!(result["status"], "refused", "{branch}: {result}");
    }
    for branch in ["-f", "a..b", "x y", "x.lock"] {
        let result = rig.call("git_push", json!({"branch": branch})).await;
        assert_eq!(result["status"], "error", "{branch}: {result}");
    }
    assert_eq!(credential.calls(), 0);
    // A branch git_push did not push cannot be a pull request's head.
    let result = rig
        .call(
            "open_pull_request",
            json!({"title": "t", "branch": "someone-elses", "push": false}),
        )
        .await;
    assert_eq!(result["status"], "refused", "{result}");
    // Nothing to push.
    let empty = Rig::new(
        &forge,
        Some(github_credential()),
        Vec::new(),
        Source {
            answer: Some(PushUnavailable::Empty),
            ..source("https://github.example.test/EliteaAI/elitea-platform.git")
        },
    )
    .await;
    let result = empty.call("git_push", json!({"branch": "x"})).await;
    assert_eq!(result["status"], "refused", "{result}");
    assert!(forge.hits().is_empty(), "nothing reached the forge");
    assert!(rig.audit.pushed.lock().expect("audit").is_empty());
}

#[tokio::test]
async fn a_refused_push_or_credential_is_reported_and_not_audited() {
    let remote = "https://github.example.test/EliteaAI/elitea-platform.git";
    let forge = Forge::with_branches(&["main"]);
    let mut declined = Vec::new();
    pkt(&mut declined, "unpack ok\n");
    pkt(&mut declined, "ng refs/heads/x pre-receive hook declined\n");
    declined.extend_from_slice(b"0000");
    *forge.report.lock().expect("report") = Some(declined);
    let rig = Rig::new(
        &forge,
        Some(github_credential()),
        Vec::new(),
        source(remote),
    )
    .await;
    let result = rig
        .call("open_pull_request", json!({"title": "t", "branch": "x"}))
        .await;
    assert_eq!(result["status"], "error", "{result}");
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("pre-receive hook declined")
    );
    assert!(
        forge.hits().iter().all(|hit| !hit.url.ends_with("/pulls")),
        "no pull request without a push"
    );
    assert!(rig.audit.pushed.lock().expect("audit").is_empty());
    assert!(rig.audit.opened.lock().expect("audit").is_empty());

    let forge = Forge::with_branches(&["main"]);
    *forge.git_status.lock().expect("status") = Some(401);
    let rig = Rig::new(
        &forge,
        Some(github_credential()),
        Vec::new(),
        source(remote),
    )
    .await;
    let result = rig.call("git_push", json!({"branch": "x"})).await;
    assert_eq!(result["status"], "error", "{result}");
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("credential")
    );
    assert_eq!(forge.hits().len(), 1, "no pack after a refused credential");
}

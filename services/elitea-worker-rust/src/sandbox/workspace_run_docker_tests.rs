//! Opt-in proofs of workspace runs against the real Linux Docker runtime
//! (ADR-0034 decision 9), run by the `workspace-run-docker` job in
//! `.github/workflows/ci-rust.yml` with the runner image built there:
//!
//! * the supervisor bridge end to end: the worker's launcher, real gRPC, the
//!   supervisor's service and its Docker backend, the real
//!   `elitea-workspace-runner` image (start, unpack, serve, pack, destroy);
//! * the egress proof: with the project's network on, public HTTPS and DNS
//!   work and RFC 1918, link-local metadata and the platform's own service
//!   are unreachable; with it off, there is no network and no DNS.
//!
//! `ELITEA_WORKSPACE_RUNNER_TEST_IMAGE` names the image by digest
//! (`name@sha256:…`). The egress test also needs the dedicated network that
//! `deploy/docker/workspace-egress.sh` created
//! (`ELITEA_WORKSPACE_EGRESS_NETWORK`, `ELITEA_WORKSPACE_EGRESS_DNS`) and a
//! listener standing in for Main (`ELITEA_WORKSPACE_EGRESS_PLATFORM_URL`).
//! * the proxy proof (host-allowlist mode): on the internal proxy network,
//!   with the run token in the command environment, an allowed host is
//!   reachable through the real egress proxy, and a host off the allowlist,
//!   a direct connection, DNS and a fronted (SNI-mismatched) tunnel are not
//!   (`ELITEA_WORKSPACE_PROXY_NETWORK`, `ELITEA_WORKSPACE_PROXY_URL`,
//!   `ELITEA_WORKSPACE_PROXY_TOKEN_FILE`).
//!
//! `ELITEA_REQUIRE_WORKSPACE_DOCKER_TESTS=1` turns a missing variable into a
//! failure, so a skipped proof never reads as a passing one.

use std::sync::Arc;

use serde_json::{Value, json};

use super::workspace_run_docker::DockerWorkspaceRuns;
use crate::workspace_run::redact::SecretEnv;
use crate::workspace_run::sandbox::WorkspaceSandboxLauncher as _;
use crate::workspace_run::sandbox::{WorkspaceRunGrants, WorkspaceSandbox};
use crate::workspace_run::supervisor::tests::{TestGrants, serve, spec};

fn required(name: &str) -> Option<String> {
    match std::env::var(name) {
        Ok(value) if !value.is_empty() => Some(value),
        _ => {
            assert!(
                std::env::var("ELITEA_REQUIRE_WORKSPACE_DOCKER_TESTS").as_deref() != Ok("1"),
                "{name} is required by this job"
            );
            eprintln!("{name} is not set; the Docker workspace-run proof is skipped");
            None
        }
    }
}

fn exit_code(value: &Value) -> Option<i64> {
    match value {
        Value::Object(map) => map
            .get("exit_code")
            .and_then(Value::as_i64)
            .or_else(|| map.values().find_map(exit_code)),
        Value::Array(items) => items.iter().find_map(exit_code),
        _ => None,
    }
}

async fn run(
    sandbox: &Arc<dyn WorkspaceSandbox>,
    call: &str,
    command: &str,
    network: bool,
) -> Value {
    sandbox
        .call_tool(
            "run_command",
            call,
            json!({"command": command, "sandbox": "workspace-write", "network": network, "timeout_seconds": 30}),
        )
        .await
        .expect("the runner answered")
}

async fn succeeds(
    sandbox: &Arc<dyn WorkspaceSandbox>,
    call: &str,
    command: &str,
    network: bool,
) -> bool {
    let answer = run(sandbox, call, command, network).await;
    eprintln!("{command}: {answer}");
    exit_code(&answer) == Some(0)
}

async fn launch(
    egress_network: Option<String>,
    egress_dns: Vec<String>,
    image: String,
    egress: bool,
    run: &str,
) -> (Arc<dyn WorkspaceSandbox>, tokio::task::JoinHandle<()>) {
    let network = if egress {
        crate::workspace_run::sandbox::WorkspaceNetwork::PublicEgress
    } else {
        crate::workspace_run::sandbox::WorkspaceNetwork::None
    };
    let env = SecretEnv::new(vec![("CI_TOKEN".into(), "tok-workspace-ci".into())]);
    launch_with(egress_network, egress_dns, None, image, network, env, run).await
}

async fn launch_with(
    egress_network: Option<String>,
    egress_dns: Vec<String>,
    proxy_network: Option<String>,
    image: String,
    network: crate::workspace_run::sandbox::WorkspaceNetwork,
    env: SecretEnv,
    run: &str,
) -> (Arc<dyn WorkspaceSandbox>, tokio::task::JoinHandle<()>) {
    let backend = DockerWorkspaceRuns::connect(egress_network, egress_dns, proxy_network)
        .await
        .expect("Docker is reachable");
    let (_supervisor, launcher, server) = serve(Arc::new(backend), image, 4).await;
    let mut spec = spec(run);
    spec.shell = true;
    spec.network = network;
    let sandbox = launcher
        .launch(
            &spec,
            &env,
            TestGrants::new("execution-docker") as Arc<dyn WorkspaceRunGrants>,
        )
        .await
        .expect("the run started");
    (sandbox, server)
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires Docker and ELITEA_WORKSPACE_RUNNER_TEST_IMAGE (ci-rust.yml workspace-run-docker)"]
async fn a_workspace_run_lives_its_turn_on_docker() {
    let Some(image) = required("ELITEA_WORKSPACE_RUNNER_TEST_IMAGE") else {
        return;
    };
    let (sandbox, _server) =
        launch(None, Vec::new(), image, false, "0a0a0a0a0a0a0a0a0a0a0a0a").await;
    let tree = tempfile::tempdir().expect("tree");
    std::fs::create_dir_all(tree.path().join("src")).expect("mkdir");
    std::fs::write(tree.path().join("src/main.rs"), "fn main() {}\n").expect("w");
    sandbox
        .push_tree(tree.path())
        .await
        .expect("unpacked into the volume");
    assert!(
        succeeds(
            &sandbox,
            "c-1",
            "test -f src/main.rs && echo built > out.txt && env > env.txt",
            false
        )
        .await
    );
    // The harvested env reached the command, and only the command.
    let printed = run(&sandbox, "c-2", "cat env.txt", false).await.to_string();
    assert!(printed.contains("CI_TOKEN="), "{printed}");
    // A destructive command is not run unapproved.
    let refused = run(&sandbox, "c-3", "rm -rf src", false).await;
    assert_ne!(exit_code(&refused), Some(0), "{refused}");
    assert!(
        sandbox
            .classify_command("rm -rf src", None)
            .await
            .expect("classified")
    );
    let out = tempfile::tempdir().expect("out");
    sandbox.pull_tree(out.path()).await.expect("packed");
    assert_eq!(
        std::fs::read_to_string(out.path().join("out.txt")).expect("out.txt"),
        "built\n"
    );
    assert!(out.path().join("src/main.rs").exists());
    sandbox.destroy().await.expect("destroyed");
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires Docker, the runner image and the workspace egress network (ci-rust.yml workspace-run-docker)"]
async fn workspace_egress_is_the_public_internet_only_and_off_is_nothing() {
    let (Some(image), Some(network), Some(dns), Some(platform)) = (
        required("ELITEA_WORKSPACE_RUNNER_TEST_IMAGE"),
        required("ELITEA_WORKSPACE_EGRESS_NETWORK"),
        required("ELITEA_WORKSPACE_EGRESS_DNS"),
        required("ELITEA_WORKSPACE_EGRESS_PLATFORM_URL"),
    ) else {
        return;
    };
    let dns: Vec<String> = dns.split(',').map(str::to_owned).collect();
    let curl = |url: &str| format!("curl -sS -o /dev/null --connect-timeout 5 --max-time 10 {url}");

    let (on, _server) = launch(
        Some(network.clone()),
        dns.clone(),
        image.clone(),
        true,
        "0b0b0b0b0b0b0b0b0b0b0b0b",
    )
    .await;
    assert!(
        succeeds(&on, "e-1", &curl("https://example.com"), true).await,
        "public HTTPS"
    );
    assert!(
        succeeds(&on, "e-2", "getent hosts example.com", true).await,
        "DNS"
    );
    for denied in [
        "http://10.0.0.1/",
        "http://172.16.0.1/",
        "http://192.168.0.1/",
        "http://169.254.169.254/latest/meta-data/",
        platform.as_str(),
    ] {
        assert!(
            !succeeds(&on, "e-3", &curl(denied), true).await,
            "{denied} is reachable"
        );
    }
    on.destroy().await.expect("destroyed");

    let (off, _server) = launch(Some(network), dns, image, false, "0c0c0c0c0c0c0c0c0c0c0c0c").await;
    assert!(
        !succeeds(&off, "o-1", &curl("https://example.com"), false).await,
        "no network"
    );
    assert!(
        !succeeds(&off, "o-2", "getent hosts example.com", false).await,
        "no DNS"
    );
    assert!(
        !succeeds(&off, "o-3", &curl(&platform), false).await,
        "no platform"
    );
    off.destroy().await.expect("destroyed");
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires Docker, the runner image, the proxy network and a running egress proxy (ci-rust.yml workspace-run-docker)"]
async fn proxy_mode_reaches_only_allowed_hosts_through_the_egress_proxy() {
    let (Some(image), Some(proxy_network), Some(proxy_url), Some(token_file), Some(platform)) = (
        required("ELITEA_WORKSPACE_RUNNER_TEST_IMAGE"),
        required("ELITEA_WORKSPACE_PROXY_NETWORK"),
        required("ELITEA_WORKSPACE_PROXY_URL"),
        required("ELITEA_WORKSPACE_PROXY_TOKEN_FILE"),
        required("ELITEA_WORKSPACE_EGRESS_PLATFORM_URL"),
    ) else {
        return;
    };
    let token = std::fs::read_to_string(token_file).expect("the token file");
    let token = token.trim();
    // The worker's own path: resolve the proxy, put the token in the
    // command environment only.
    let url = crate::workspace_run::turn::proxy_env_url(&proxy_url, token)
        .await
        .expect("the proxy URL");
    let mut env = SecretEnv::new(Vec::new());
    env.set_egress_proxy(&url, token);
    let redactor = env.redactor();
    let (sandbox, _server) = launch_with(
        None,
        Vec::new(),
        Some(proxy_network),
        image,
        crate::workspace_run::sandbox::WorkspaceNetwork::Proxy,
        env,
        "0d0d0d0d0d0d0d0d0d0d0d0d",
    )
    .await;
    let curl = |extra: &str, url: &str| {
        format!("curl -sS -o /dev/null --connect-timeout 5 --max-time 15 {extra} {url}")
    };
    // The allowlist the CI token carries: example.com only.
    assert!(
        succeeds(&sandbox, "p-1", &curl("", "https://example.com"), true).await,
        "an allowed host through the proxy"
    );
    assert!(
        !succeeds(&sandbox, "p-2", &curl("", "https://www.iana.org"), true).await,
        "a host off the allowlist"
    );
    assert!(
        !succeeds(
            &sandbox,
            "p-3",
            &curl("--noproxy '*'", "https://example.com"),
            true
        )
        .await,
        "a direct connection without the proxy"
    );
    assert!(
        !succeeds(&sandbox, "p-4", "getent hosts example.com", true).await,
        "DNS"
    );
    // Domain fronting: CONNECT to the allowed host, TLS server name of
    // another. The proxy refuses the ClientHello (sni-mismatch).
    assert!(
        !succeeds(
            &sandbox,
            "p-5",
            &curl(
                "--connect-to www.iana.org:443:example.com:443",
                "https://www.iana.org"
            ),
            true
        )
        .await,
        "an SNI that is not the CONNECT host"
    );
    assert!(
        !succeeds(&sandbox, "p-6", &curl("--noproxy '*'", &platform), true).await,
        "the platform, directly"
    );
    // The token is in the command environment and never in a result.
    let printed = run(&sandbox, "p-7", "env", true).await;
    let redacted = redactor.json(printed).to_string();
    assert!(!redacted.contains(token), "the token leaked: {redacted}");
    assert!(redacted.contains("HTTPS_PROXY="), "{redacted}");
    sandbox.destroy().await.expect("destroyed");
}

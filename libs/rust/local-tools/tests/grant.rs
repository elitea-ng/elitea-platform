//! A rule's grant on the real Seatbelt sandbox (#3005, #3006): a command
//! granted the `gh` login and named hosts gets the login in its own
//! environment only, reads no credential file, writes nothing into the
//! grant's configuration directory, reaches no host but through the
//! filtering proxy (which refuses unlisted hosts and records them), and
//! the login never shows in its output.
//!
//! macOS only. The `gh` here is a fake on `PATH`: the test runs itself
//! again as a child process with that `PATH` (changing the environment of
//! this process would need `unsafe`). Where `sandbox-exec` is missing it
//! skips, unless `ELITEA_REQUIRE_SEATBELT=1` (CI's macOS job).
#![cfg(target_os = "macos")]

use std::os::unix::fs::PermissionsExt as _;
use std::path::Path;

use elitea_local_tools::credentials::CommandGrant;
use elitea_local_tools::policy::SandboxMode;
use elitea_local_tools::shell::{CommandSpec, ShellConfig, run};
use elitea_local_tools::workspace::{Workspace, WsPath};

const INNER: &str = "ELITEA_GRANT_TEST_INNER";
const TOKEN: &str = "gho_granttesttoken0123456789";

#[test]
fn a_granted_command_runs_sandboxed_with_its_login_only() {
    if !Path::new("/usr/bin/sandbox-exec").is_file() {
        assert!(
            std::env::var_os("ELITEA_REQUIRE_SEATBELT").is_none(),
            "ELITEA_REQUIRE_SEATBELT is set but /usr/bin/sandbox-exec is missing"
        );
        eprintln!("skipping: /usr/bin/sandbox-exec is missing");
        return;
    }
    let bin = tempfile::tempdir().expect("bin");
    let gh = bin.path().join("gh");
    // Outside the sandbox (the host reading the login) it prints the
    // token; inside it reports what it can see and do.
    std::fs::write(
        &gh,
        format!(
            r#"#!/bin/sh
if [ "$1 $2" = "auth token" ]; then echo {TOKEN}; exit 0; fi
echo "token=$GH_TOKEN"
echo "config=$GH_CONFIG_DIR"
echo "notifier=$GH_NO_UPDATE_NOTIFIER"
echo "proxy_vars=$(env | grep -ci '_proxy=')"
touch "$GH_CONFIG_DIR/planted" 2>/dev/null && echo "config=writable" || echo "config=readonly"
cat "$HOME/.config/gh/hosts.yml" 2>/dev/null && echo "hosts=read" || echo "hosts=hidden"
/usr/bin/curl -sS -m 5 -o /dev/null https://evil.example.com 2>&1 | head -1
/usr/bin/curl -sS -m 5 --noproxy '*' -o /dev/null https://140.82.112.6 2>&1 | head -1
echo "local=$(/usr/bin/curl -sS -m 3 --noproxy '*' -o /dev/null "http://127.0.0.1:$3/" 2>&1 | head -1)"
echo "end"
"#
        ),
    )
    .expect("gh");
    std::fs::set_permissions(&gh, std::fs::Permissions::from_mode(0o755)).expect("chmod");
    let path = format!(
        "{}:{}",
        bin.path().display(),
        std::env::var("PATH").unwrap_or_default()
    );
    let status = std::process::Command::new(std::env::current_exe().expect("exe"))
        .args(["--ignored", "--exact", "inner", "--nocapture"])
        .env("PATH", path)
        .env(INNER, "1")
        .status()
        .expect("inner");
    assert!(status.success(), "the inner test failed");
}

#[tokio::test]
#[ignore = "run by a_granted_command_runs_sandboxed_with_its_login_only"]
async fn inner() {
    if std::env::var_os(INNER).is_none() {
        return;
    }
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("workspace");
    let home = base.join("home");
    std::fs::create_dir_all(&root).expect("workspace");
    std::fs::create_dir_all(home.join(".config/gh")).expect("gh config");
    std::fs::write(
        home.join(".config/gh/hosts.yml"),
        "oauth_token: HOSTS-SECRET\n",
    )
    .expect("hosts");
    let workspace = Workspace::open(&root, &[]).expect("open");
    let data = base.join("data");
    let mut config = ShellConfig::new(data.join("tmp/session"));
    config.deny_read.push(data);
    config.home = Some(home);
    config.credential_grants = true;
    let grant = CommandGrant {
        rule: "user#1".to_owned(),
        credential: Some("gh".to_owned()),
        hosts: vec!["api.github.com".to_owned()],
    };
    // Another local listener: the sandbox reaches the proxy's port only.
    let other = std::net::TcpListener::bind("127.0.0.1:0").expect("listener");
    let other_port = other.local_addr().expect("addr").port();
    let spec = CommandSpec {
        command: format!("gh pr list {other_port}"),
        cwd: WsPath::root(),
        timeout: None,
        mode: SandboxMode::WorkspaceWrite,
        network: true,
        loopback: false,
        grant: Some(grant.clone()),
    };
    let output = run(&workspace, &config, &spec).await.expect("ran");
    let all = format!("{}\n{}", output.stdout, output.stderr);
    assert!(!all.contains(TOKEN), "the token leaked: {all}");
    assert!(all.contains("token=[REDACTED]"), "{all}");
    assert!(all.contains("notifier=1"), "{all}");
    assert!(all.contains("proxy_vars=6"), "{all}");
    assert!(all.contains("config=readonly"), "{all}");
    assert!(
        all.contains("hosts=hidden") && !all.contains("HOSTS-SECRET"),
        "{all}"
    );
    assert!(
        all.contains("403"),
        "the proxy refuses an unlisted host: {all}"
    );
    // Refused at connect (the listener would accept, then time out).
    let local = all
        .lines()
        .find(|line| line.starts_with("local="))
        .unwrap_or_default();
    assert!(
        local.contains("Failed to connect") || local.contains("Couldn't connect"),
        "another local port: {local}"
    );
    assert!(all.contains("end"), "{all}");
    let network = output.network.expect("a network report");
    assert_eq!(network.allowed_hosts, vec!["api.github.com".to_owned()]);
    assert_eq!(
        network.refused_hosts,
        vec!["evil.example.com:443".to_owned()]
    );
    assert_eq!(
        output.grant.as_deref(),
        Some("the gh login, network limited to api.github.com")
    );

    // Off on this host: refused, nothing ran.
    config.credential_grants = false;
    let refused = run(&workspace, &config, &spec).await.expect_err("refused");
    assert!(
        refused.message().contains("nothing ran"),
        "{}",
        refused.message()
    );
    config.credential_grants = true;

    // Not a command the profile hands its login to: refused.
    let auth = CommandSpec {
        command: "gh auth token".to_owned(),
        ..spec.clone()
    };
    assert!(run(&workspace, &config, &auth).await.is_err());
    // A compound command never gets a grant.
    let compound = CommandSpec {
        command: "gh pr list | cat".to_owned(),
        ..spec.clone()
    };
    assert!(run(&workspace, &config, &compound).await.is_err());
    // A hosts-only grant: the proxy, no login.
    let curl = CommandSpec {
        command: "/usr/bin/curl -sS -m 5 -o /dev/null https://evil.example.com".to_owned(),
        grant: Some(CommandGrant {
            credential: None,
            ..grant
        }),
        ..spec
    };
    let output = run(&workspace, &config, &curl).await.expect("ran");
    assert_ne!(output.exit_code, Some(0));
    assert!(output.stderr.contains("403"), "{}", output.stderr);
}

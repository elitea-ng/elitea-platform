//! The macOS Seatbelt sandbox, proven on the real `sandbox-exec`: writes
//! outside the workspace and network connections (loopback included) are
//! blocked, and allowed again when the mode or the network switch says so;
//! every `.git` entry stays read-only; credentials, the host's data
//! directory and `path_deny` files cannot be read; nothing listens.
//!
//! macOS only; elsewhere this file has no tests. Where `sandbox-exec` is
//! missing each test skips, unless `ELITEA_REQUIRE_SEATBELT=1` (CI's macOS
//! job) turns the skip into a failure.
#![cfg(target_os = "macos")]

use std::net::TcpListener;
use std::path::{Path, PathBuf};

use elitea_local_tools::policy::SandboxMode;
use elitea_local_tools::sandbox::Enforcement;
use elitea_local_tools::shell::{CommandOutput, CommandSpec, ShellConfig, run};
use elitea_local_tools::workspace::{Workspace, WsPath};

/// The desktop app's bundle identifier, as its host passes it.
const APP_ID: &str = "ai.elitea.desktop";

fn available() -> bool {
    if Path::new("/usr/bin/sandbox-exec").is_file() {
        return true;
    }
    assert!(
        std::env::var_os("ELITEA_REQUIRE_SEATBELT").is_none(),
        "ELITEA_REQUIRE_SEATBELT is set but /usr/bin/sandbox-exec is missing"
    );
    eprintln!("skipping: /usr/bin/sandbox-exec is missing");
    false
}

struct Fixture {
    _dir: tempfile::TempDir,
    /// The commands' `HOME` and the home credentials are denied under: a
    /// fixture, so what the probes look for really exists.
    home: PathBuf,
    outside: PathBuf,
    workspace: Workspace,
    config: ShellConfig,
}

fn fixture() -> Fixture {
    fixture_with(&[])
}

fn fixture_with(path_deny: &[&str]) -> Fixture {
    let dir = tempfile::tempdir().expect("dir");
    let data = dir.path().join("data");
    fixture_in(dir, path_deny, |_| data)
}

/// A fixture whose session data directory is `data(home)`.
fn fixture_in(
    dir: tempfile::TempDir,
    path_deny: &[&str],
    data: impl FnOnce(&Path) -> PathBuf,
) -> Fixture {
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("workspace");
    let outside = base.join("outside");
    let home = base.join("home");
    std::fs::create_dir_all(root.join(".git/hooks")).expect("workspace");
    std::fs::create_dir_all(&outside).expect("outside");
    std::fs::create_dir_all(&home).expect("home");
    let deny: Vec<String> = path_deny.iter().map(|s| (*s).to_owned()).collect();
    let workspace = Workspace::open(&root, &deny).expect("open");
    let data = data(&home);
    let mut config = ShellConfig::new(data.join("tmp/session"));
    config.deny_read.push(data);
    config.home = Some(home.clone());
    // The host names its identifier; the library holds none.
    config.app_id = Some(APP_ID.to_owned());
    Fixture {
        outside,
        home,
        _dir: dir,
        workspace,
        config,
    }
}

/// Write `content` at `path`, creating its parents.
fn plant(path: &Path, content: &str) {
    std::fs::create_dir_all(path.parent().expect("parent")).expect("parents");
    std::fs::write(path, content).expect("plant");
}

/// `probe` ran, was refused by the sandbox (not "no such file"), and
/// printed nothing of `secret`.
async fn assert_refused(fixture: &Fixture, probe: &str, secret: &str) {
    let output = sh(fixture, probe, SandboxMode::WorkspaceWrite, false).await;
    assert_ne!(
        output.exit_code,
        Some(0),
        "`{probe}` read: {}",
        output.stdout
    );
    assert!(
        !output.stdout.contains(secret) && !output.stderr.contains(secret),
        "`{probe}` leaked {secret}"
    );
    assert!(
        output.stderr.contains("Operation not permitted"),
        "`{probe}` failed for another reason: {}",
        output.stderr
    );
}

async fn sh(fixture: &Fixture, command: &str, mode: SandboxMode, network: bool) -> CommandOutput {
    sh_loopback(fixture, command, mode, network, false).await
}

async fn sh_loopback(
    fixture: &Fixture,
    command: &str,
    mode: SandboxMode,
    network: bool,
    loopback: bool,
) -> CommandOutput {
    let output = run(
        &fixture.workspace,
        &fixture.config,
        &CommandSpec {
            command: command.to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode,
            network,
            loopback,
        },
    )
    .await
    .expect("run");
    let expected = if mode == SandboxMode::FullAccess && network {
        Enforcement::None
    } else {
        Enforcement::Full
    };
    assert_eq!(output.enforcement, expected);
    output
}

fn quoted(path: &Path) -> String {
    format!("'{}'", path.display())
}

#[tokio::test]
async fn workspace_write_confines_writes_to_the_workspace_and_temp_dir() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let inside = fixture.workspace.root().join("made-inside");
    let outside = fixture.outside.join("made-outside");
    let tmp = sh(
        &fixture,
        "touch \"$TMPDIR/scratch\"",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(tmp.exit_code, Some(0), "{}", tmp.stderr);

    let ok = sh(
        &fixture,
        &format!("touch {}", quoted(&inside)),
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(ok.exit_code, Some(0), "{}", ok.stderr);
    assert!(inside.exists());

    let blocked = sh(
        &fixture,
        &format!("touch {}", quoted(&outside)),
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_ne!(blocked.exit_code, Some(0));
    assert!(
        blocked.stderr.contains("Operation not permitted"),
        "{}",
        blocked.stderr
    );
    assert!(
        !outside.exists(),
        "the sandbox let a write outside the workspace through"
    );

    let full = sh(
        &fixture,
        &format!("touch {}", quoted(&outside)),
        SandboxMode::FullAccess,
        false,
    )
    .await;
    assert_eq!(full.exit_code, Some(0), "{}", full.stderr);
    assert!(outside.exists());
}

#[tokio::test]
async fn read_only_blocks_every_write_but_reads_work() {
    if !available() {
        return;
    }
    let fixture = fixture();
    std::fs::write(fixture.workspace.root().join("readme"), "hello").expect("seed");
    let read = sh(&fixture, "cat readme", SandboxMode::ReadOnly, false).await;
    assert_eq!(read.stdout, "hello");
    let write = sh(&fixture, "touch new-file", SandboxMode::ReadOnly, false).await;
    assert_ne!(write.exit_code, Some(0));
    assert!(!fixture.workspace.root().join("new-file").exists());
    let devnull = sh(
        &fixture,
        "echo quiet > /dev/null",
        SandboxMode::ReadOnly,
        false,
    )
    .await;
    assert_eq!(devnull.exit_code, Some(0), "{}", devnull.stderr);
}

/// H1: not only hooks and config: the whole `.git`, any `.git` at any
/// depth, under any spelling, cannot be moved, replaced or written, and no
/// new one can be planted.
#[tokio::test]
async fn git_directories_stay_read_only_at_every_depth_and_spelling() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let root = fixture.workspace.root().to_path_buf();
    std::fs::write(root.join(".git/config"), "[core]\n").expect("config");
    std::fs::create_dir_all(root.join("vendor/lib/.git")).expect("nested");
    for probe in [
        "mv .git .gitold",
        "rm -rf .git",
        "touch .git/commondir",
        "touch .git/index",
        "mkdir -p .git/modules/x && touch .git/modules/x/config",
        "touch .git/hooks/pre-commit",
        "echo '[filter \"x\"]' >> .git/config",
        "touch .GIT/hooks/post-checkout",
        "touch .Git/config",
        "touch vendor/lib/.git/config",
        "mkdir sub && mkdir sub/.git",
        "mkdir -p sub2 && echo 'gitdir: /tmp/evil' > sub2/.git",
        "ln -s /tmp sub3 && mv sub3 .git2 && mv .git2 .git",
    ] {
        let output = sh(&fixture, probe, SandboxMode::WorkspaceWrite, false).await;
        assert_ne!(output.exit_code, Some(0), "`{probe}` succeeded");
    }
    assert!(root.join(".git").is_dir(), ".git is where it was");
    assert_eq!(
        std::fs::read_to_string(root.join(".git/config")).expect("config"),
        "[core]\n"
    );
    for planted in [
        ".git/commondir",
        ".git/index",
        ".git/modules",
        ".git/hooks/pre-commit",
        "vendor/lib/.git/config",
        "sub/.git",
        "sub2/.git",
    ] {
        assert!(!root.join(planted).exists(), "{planted} was created");
    }
    let normal = sh(
        &fixture,
        "mkdir -p src && touch src/lib.rs .gitignore .github",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(normal.exit_code, Some(0), "{}", normal.stderr);
}

/// M3: reads are confined too: `path_deny` files in the workspace, the
/// host's data directory and the person's credentials cannot be read,
/// though they stay listed; everything else reads as before.
#[tokio::test]
async fn path_deny_credentials_and_the_data_directory_are_unreadable() {
    if !available() {
        return;
    }
    let fixture = fixture_with(&[".env", "secrets/**"]);
    let root = fixture.workspace.root().to_path_buf();
    std::fs::write(root.join(".env"), "TOKEN=1").expect("env");
    std::fs::create_dir_all(root.join("secrets")).expect("secrets");
    std::fs::write(root.join("secrets/key"), "k").expect("key");
    std::fs::write(root.join("readme"), "hello").expect("readme");
    let data = fixture.config.deny_read.first().cloned().expect("data dir");
    std::fs::create_dir_all(data.join("checkpoints")).expect("data");
    std::fs::write(data.join("checkpoints/manifest.json"), "{}").expect("manifest");
    let data = std::fs::canonicalize(data).expect("canonical");

    for probe in [
        "cat .env".to_owned(),
        "cat ./sub/../.env".to_owned(),
        "cat secrets/key".to_owned(),
        "cp .env leaked".to_owned(),
        "echo x > .env".to_owned(),
        format!("cat {}", quoted(&data.join("checkpoints/manifest.json"))),
    ] {
        let output = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, false).await;
        assert_ne!(
            output.exit_code,
            Some(0),
            "`{probe}` read: {}",
            output.stdout
        );
        assert!(!output.stdout.contains("TOKEN"), "{probe}");
    }
    assert_eq!(
        std::fs::read_to_string(root.join(".env")).expect("env"),
        "TOKEN=1"
    );
    let listed = sh(&fixture, "ls -a", SandboxMode::ReadOnly, false).await;
    assert!(listed.stdout.contains(".env"), "metadata stays visible");
    let read = sh(&fixture, "cat readme", SandboxMode::ReadOnly, false).await;
    assert_eq!(read.stdout, "hello");
    let tmp = sh(
        &fixture,
        "echo t > \"$TMPDIR/x\" && cat \"$TMPDIR/x\"",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(
        tmp.stdout.trim(),
        "t",
        "the temporary directory inside the data directory stays usable: {}",
        tmp.stderr
    );
    let full = sh(&fixture, "cat .env", SandboxMode::FullAccess, false).await;
    assert_ne!(full.exit_code, Some(0), "full access still keeps path_deny");
}

/// The desktop app's own data under its bundle identifier (the stored
/// sign-in, other workspaces' history), the keychains, and a directory the
/// host resolved for itself (an `XDG_CONFIG_HOME` the defaults miss) all
/// exist with content, and every read is refused by the sandbox: not
/// "No such file", and nothing of the content comes out.
#[tokio::test]
async fn the_desktop_apps_data_and_host_resolved_dirs_are_unreadable() {
    if !available() {
        return;
    }
    let mut fixture = fixture();
    let home = fixture.home.clone();
    let app = home.join("Library/Application Support/ai.elitea.desktop");
    plant(&app.join("credentials.json"), "REFRESH-SECRET-1");
    plant(&app.join("threads.sqlite"), "THREADS-SECRET-2");
    plant(
        &home.join("Library/Preferences/ai.elitea.desktop.plist"),
        "PLIST-SECRET-3",
    );
    plant(
        &home.join("Library/HTTPStorages/ai.elitea.desktop/httpstorages.sqlite"),
        "COOKIE-SECRET-4",
    );
    plant(
        &home.join("Library/Keychains/login.keychain-db"),
        "KEYCHAIN-SECRET-5",
    );
    // The host-resolved config directory, outside every default layout.
    let resolved = fixture.outside.join("xdg-config/ai.elitea.desktop");
    plant(&resolved.join("credentials.json"), "HOST-SECRET-6");
    fixture.config.deny_read.push(resolved.clone());

    for (probe, secret) in [
        (
            "cat \"$HOME/Library/Application Support/ai.elitea.desktop/credentials.json\""
                .to_owned(),
            "REFRESH-SECRET-1",
        ),
        (
            format!("cat {}", quoted(&app.join("threads.sqlite"))),
            "THREADS-SECRET-2",
        ),
        (
            "cat \"$HOME/Library/Preferences/ai.elitea.desktop.plist\"".to_owned(),
            "PLIST-SECRET-3",
        ),
        (
            "cat \"$HOME/Library/HTTPStorages/ai.elitea.desktop/httpstorages.sqlite\"".to_owned(),
            "COOKIE-SECRET-4",
        ),
        (
            "cat \"$HOME/Library/Keychains/login.keychain-db\"".to_owned(),
            "KEYCHAIN-SECRET-5",
        ),
        (
            format!("cat {}", quoted(&resolved.join("credentials.json"))),
            "HOST-SECRET-6",
        ),
        (
            format!("cp {} leaked", quoted(&resolved.join("credentials.json"))),
            "HOST-SECRET-6",
        ),
    ] {
        assert_refused(&fixture, &probe, secret).await;
    }
    assert!(!fixture.workspace.root().join("leaked").exists());
    // Metadata stays visible: the directory shows in its parent's listing.
    let listed = sh(
        &fixture,
        "ls \"$HOME/Library/Application Support\"",
        SandboxMode::ReadOnly,
        false,
    )
    .await;
    assert!(
        listed.stdout.contains("ai.elitea.desktop"),
        "{}",
        listed.stderr
    );
}

/// The session's data directory sits inside the desktop app's own data
/// directory, which is denied: the temporary directory inside it stays
/// writable and readable, and the stored sign-in next to it does not read.
#[tokio::test]
async fn the_temp_dir_inside_the_denied_app_data_stays_usable() {
    if !available() {
        return;
    }
    let dir = tempfile::tempdir().expect("dir");
    let fixture = fixture_in(dir, &[], |home| {
        home.join("Library/Application Support/ai.elitea.desktop/workspaces/w1")
    });
    let app = fixture
        .home
        .join("Library/Application Support/ai.elitea.desktop");
    plant(&app.join("credentials.json"), "REFRESH-SECRET-7");
    let tmp = sh(
        &fixture,
        "echo t > \"$TMPDIR/x\" && cat \"$TMPDIR/x\"",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(tmp.exit_code, Some(0), "{}", tmp.stderr);
    assert_eq!(tmp.stdout.trim(), "t");
    assert!(
        fixture.config.temp_dir.starts_with(&app),
        "the temporary directory is under the app's data"
    );
    assert_refused(
        &fixture,
        &format!("cat {}", quoted(&app.join("credentials.json"))),
        "REFRESH-SECRET-7",
    )
    .await;
}

/// L2: with the network on, connections go out but nothing listens.
#[tokio::test]
async fn network_on_still_refuses_listening_sockets() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let listen = sh(
        &fixture,
        "/usr/bin/nc -l 127.0.0.1 0",
        SandboxMode::WorkspaceWrite,
        true,
    )
    .await;
    assert_ne!(listen.exit_code, Some(0), "a sandboxed command listened");
}

#[tokio::test]
async fn network_is_blocked_when_denied_and_open_when_allowed() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
    let port = listener.local_addr().expect("addr").port();
    let probe = format!("/usr/bin/nc -z -G 3 127.0.0.1 {port}");
    let denied = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, false).await;
    assert_ne!(
        denied.exit_code,
        Some(0),
        "a denied sandbox reached loopback: {}",
        denied.stdout
    );
    let allowed = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, true).await;
    assert_eq!(allowed.exit_code, Some(0), "{}", allowed.stderr);
    drop(listener);
}

/// #1223: without the loopback switch a command reaches nothing on
/// localhost, its own listener included.
#[tokio::test]
async fn loopback_off_denies_localhost_connect() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
    let port = listener.local_addr().expect("addr").port();
    let probe = format!("/usr/bin/nc -z -G 3 127.0.0.1 {port}");
    let denied = sh_loopback(&fixture, &probe, SandboxMode::WorkspaceWrite, false, false).await;
    assert_ne!(denied.exit_code, Some(0), "{}", denied.stdout);
    let own = sh_loopback(
        &fixture,
        "/usr/bin/nc -l 127.0.0.1 0",
        SandboxMode::WorkspaceWrite,
        false,
        false,
    )
    .await;
    assert_ne!(own.exit_code, Some(0), "listened without loopback");
    drop(listener);
}

/// #1223: with the loopback switch a command serves and connects on
/// localhost, and still reaches nothing else.
#[tokio::test]
async fn loopback_on_allows_localhost_but_not_external() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
    let port = listener.local_addr().expect("addr").port();
    let probe = format!("/usr/bin/nc -z -G 3 127.0.0.1 {port}");
    let reached = sh_loopback(&fixture, &probe, SandboxMode::WorkspaceWrite, false, true).await;
    assert_eq!(reached.exit_code, Some(0), "{}", reached.stderr);
    drop(listener);

    // Its own server and client in one command.
    let served = sh_loopback(
        &fixture,
        "/usr/bin/python3 -c 'import socket; s = socket.socket(); s.bind((\"127.0.0.1\", 0)); \
         s.listen(1); c = socket.create_connection(s.getsockname()); print(\"served\")'",
        SandboxMode::WorkspaceWrite,
        false,
        true,
    )
    .await;
    assert_eq!(served.exit_code, Some(0), "{}", served.stderr);
    assert!(served.stdout.contains("served"), "{}", served.stdout);

    let external = sh_loopback(
        &fixture,
        "/usr/bin/nc -z -G 3 1.1.1.1 443",
        SandboxMode::WorkspaceWrite,
        false,
        true,
    )
    .await;
    assert_ne!(external.exit_code, Some(0), "loopback reached the network");
}

/// M5: checkpoint refs and objects live in `.git`, so a sandboxed command
/// cannot forge or rewrite a checkpoint the person would restore.
#[tokio::test]
async fn checkpoint_refs_and_objects_are_out_of_reach() {
    if !available() {
        return;
    }
    let fixture = fixture();
    let root = fixture.workspace.root().to_path_buf();
    std::fs::remove_dir_all(root.join(".git")).expect("reset");
    let init = std::process::Command::new("git")
        .current_dir(&root)
        .args(["init", "-q"])
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .status()
        .expect("git init");
    assert!(init.success());
    let refs_before = std::fs::read_dir(root.join(".git/refs"))
        .expect("refs")
        .count();
    for probe in [
        "git update-ref refs/elitea/checkpoints/s/1 4b825dc642cb6eb9a060e54bf8d69288fbee4904",
        "echo x | git hash-object -w --stdin",
        "mkdir -p .git/refs/elitea/checkpoints/s && echo 0 > .git/refs/elitea/checkpoints/s/1",
        "touch .git/objects/forged",
        "touch .git/packed-refs",
    ] {
        let output = sh(&fixture, probe, SandboxMode::WorkspaceWrite, false).await;
        assert_ne!(output.exit_code, Some(0), "`{probe}` succeeded");
    }
    assert!(!root.join(".git/refs/elitea").exists());
    assert!(!root.join(".git/objects/forged").exists());
    assert_eq!(
        std::fs::read_dir(root.join(".git/refs"))
            .expect("refs")
            .count(),
        refs_before
    );
}

/// An executable script standing in for the document sidecar.
fn script(dir: &Path, body: &str) -> PathBuf {
    use std::os::unix::fs::PermissionsExt as _;
    let path = dir.join("fake-sidecar.sh");
    std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("script");
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
    path
}

fn extractor_for<'a>(
    fixture: &'a Fixture,
    program: &'a Path,
    request: &'a elitea_local_tools::sandbox::SandboxRequest,
) -> elitea_local_tools::document::SidecarExtractor<'a> {
    elitea_local_tools::document::SidecarExtractor::new(program, request, &fixture.config.sandbox)
}

/// The sidecar's profile (read-only, network off, the session's deny list)
/// refuses what a crafted document could ask the parser to do: read the
/// stored sign-in, write a file, open a socket (loopback included).
#[test]
fn the_document_sidecar_runs_read_only_without_network_or_credentials() {
    use elitea_doc_extract::{DocumentExtractor as _, PageRange};

    if !available() {
        return;
    }
    let fixture = fixture();
    let data = fixture.config.deny_read.first().cloned().expect("data dir");
    std::fs::create_dir_all(data.join("checkpoints")).expect("data");
    plant(
        &data.join("checkpoints/credentials.json"),
        "SECRET-REFRESH-TOKEN",
    );
    let data = std::fs::canonicalize(data).expect("canonical");
    let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
    listener.set_nonblocking(true).expect("nonblocking");
    let port = listener.local_addr().expect("addr").port();
    let target = fixture.workspace.root().join("written-by-sidecar");
    let program = script(
        &fixture.outside,
        &format!(
            r#"cat > /dev/null
read_out=$(cat '{creds}' 2>&1 | tr -d '"\n')
write_out=$(touch '{target}' 2>&1 | tr -d '"\n')
net_out=$(/usr/bin/nc -z -G 2 127.0.0.1 {port} >/dev/null 2>&1 && echo connected || echo blocked)
printf '{{"ok":true,"extractor":"fake","page_count":1,"pages":[{{"n":1,"text":"read=%s|write=%s|net=%s"}}]}}' "$read_out" "$write_out" "$net_out""#,
            creds = data.join("checkpoints/credentials.json").display(),
            target = target.display(),
        ),
    );
    let request = elitea_local_tools::shell::sandbox_request(
        &fixture.workspace,
        &fixture.config,
        SandboxMode::ReadOnly,
        false,
    )
    .expect("request");
    let extractor = extractor_for(&fixture, &program, &request);
    let paged = extractor
        .extract("application/pdf", b"%PDF-1.4".to_vec(), &PageRange::ALL)
        .expect("the fake sidecar answered");
    let text = &paged.pages[0].text;
    assert!(text.contains("Operation not permitted"), "{text}");
    assert!(!text.contains("SECRET-REFRESH-TOKEN"), "{text}");
    assert!(text.contains("net=blocked"), "{text}");
    assert!(
        text.contains("write=") && !target.exists(),
        "the sidecar wrote a file: {text}"
    );
    assert!(
        matches!(
            listener.accept(),
            Err(ref error) if error.kind() == std::io::ErrorKind::WouldBlock
        ),
        "the sidecar reached loopback"
    );
}

/// A sidecar that hangs is stopped at the limit, with its process group, and
/// one that dies without an answer is an error: neither takes the host down.
#[test]
fn the_document_sidecar_that_hangs_or_dies_is_an_error() {
    use elitea_doc_extract::{DocumentExtractor as _, ExtractFailure, PageRange};

    if !available() {
        return;
    }
    let fixture = fixture();
    let request = elitea_local_tools::shell::sandbox_request(
        &fixture.workspace,
        &fixture.config,
        SandboxMode::ReadOnly,
        false,
    )
    .expect("request");

    let hang = script(&fixture.outside, "sleep 30 &\nsleep 30");
    let started = std::time::Instant::now();
    let slow = extractor_for(&fixture, &hang, &request)
        .with_timeout(std::time::Duration::from_secs(1))
        .extract("application/pdf", b"x".to_vec(), &PageRange::ALL);
    assert!(matches!(slow, Err(ExtractFailure::Timeout(_))), "{slow:?}");
    assert!(started.elapsed() < std::time::Duration::from_secs(10));

    let dies = script(&fixture.outside, "exit 3");
    let crashed = extractor_for(&fixture, &dies, &request).extract(
        "application/pdf",
        b"x".to_vec(),
        &PageRange::ALL,
    );
    assert!(
        matches!(crashed, Err(ExtractFailure::Unreadable(_))),
        "{crashed:?}"
    );

    let answers = script(
        &fixture.outside,
        r#"cat > /dev/null
printf '{"ok":false,"error":"unsupported","message":"no"}'"#,
    );
    let refused = extractor_for(&fixture, &answers, &request).extract(
        "application/pdf",
        b"x".to_vec(),
        &PageRange::ALL,
    );
    assert_eq!(refused, Err(ExtractFailure::Unsupported));
}

/// Task worktrees A and B of one repository, where the desktop puts them
/// (under its denied data), and commands confined to A as A's session runs
/// them: `(fixture rooted at A, the repository's top, B)`.
fn worktree_fixture() -> (Fixture, PathBuf, PathBuf) {
    use elitea_local_tools::git::Repo;
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let home = base.join("home");
    let repo_top = base.join("repo");
    std::fs::create_dir_all(&repo_top).expect("repo");
    let git = |args: &[&str]| {
        let status = std::process::Command::new("git")
            .current_dir(&repo_top)
            .args(["-c", "user.name=T", "-c", "user.email=t@example.com"])
            .args(args)
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .status()
            .expect("git");
        assert!(status.success(), "git {args:?}");
    };
    git(&["init", "-q"]);
    std::fs::write(repo_top.join("a.txt"), "a\n").expect("a");
    git(&["add", "-A"]);
    git(&["commit", "-q", "-m", "init"]);
    let data = home.join("Library/Application Support/ai.elitea.desktop/workspaces/w1");
    let worktrees = data.join("worktrees");
    std::fs::create_dir_all(&worktrees).expect("worktrees");
    let main = Repo::discover(
        &repo_top,
        &elitea_local_tools::sandbox::SandboxConfig::default(),
        std::sync::Arc::new(elitea_local_tools::deny::DenyList::new([])),
    )
    .expect("discover")
    .expect("repo");
    let mut made = Vec::new();
    for name in ["aaaa1111", "bbbb2222"] {
        let created = elitea_local_tools::worktree::create(
            &main,
            &worktrees.join(name),
            name,
            &format!("elitea/{name}"),
            "HEAD",
        )
        .expect("create");
        made.push(created.entry.worktree);
    }
    let sibling = made.pop().expect("b");
    let own = made.pop().expect("a");
    let workspace = Workspace::open(&own, &[]).expect("open");
    let mut config = ShellConfig::new(data.join("tmp/session"));
    config.deny_read.push(data.clone());
    // What the desktop adds for a worktree session: every task worktree of
    // the workspace (its own is the writable root, opened again).
    config.deny_read.push(worktrees);
    config.home = Some(home.clone());
    config.app_id = Some(APP_ID.to_owned());
    (
        Fixture {
            outside: base.join("outside"),
            home,
            _dir: dir,
            workspace,
            config,
        },
        repo_top,
        sibling,
    )
}

/// #1235: a command in a task worktree writes its own folder, never the
/// repository's shared objects, refs or worktree admin directories.
#[tokio::test]
async fn worktree_command_cannot_write_shared_refs() {
    if !available() {
        return;
    }
    let (fixture, top, _) = worktree_fixture();
    let own = sh(
        &fixture,
        "echo mine > mine.txt && cat mine.txt a.txt",
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_eq!(own.exit_code, Some(0), "{}", own.stderr);
    assert_eq!(own.stdout, "mine\na\n");
    let common = top.join(".git");
    for probe in [
        "git update-ref refs/heads/forged HEAD".to_owned(),
        "echo x | git hash-object -w --stdin".to_owned(),
        format!("touch {}", quoted(&common.join("refs/heads/forged"))),
        format!("touch {}", quoted(&common.join("objects/forged"))),
        format!(
            "touch {}",
            quoted(&common.join("worktrees/aaaa1111/forged"))
        ),
        format!(
            "echo x > {}",
            quoted(&common.join("worktrees/aaaa1111/HEAD"))
        ),
        "echo x > .git".to_owned(),
    ] {
        let output = sh(&fixture, &probe, SandboxMode::WorkspaceWrite, false).await;
        assert_ne!(output.exit_code, Some(0), "`{probe}` succeeded");
    }
    assert!(!common.join("refs/heads/forged").exists());
    assert!(!common.join("objects/forged").exists());
    assert!(!common.join("worktrees/aaaa1111/forged").exists());
}

/// #1235: a command in task worktree A cannot read worktree B, which lives
/// beside it in the host's data.
#[tokio::test]
async fn worktree_command_cannot_read_sibling_worktree() {
    if !available() {
        return;
    }
    let (fixture, _, sibling) = worktree_fixture();
    plant(&sibling.join("secret.txt"), "SIBLING-SECRET-9");
    assert_refused(
        &fixture,
        &format!("cat {}", quoted(&sibling.join("secret.txt"))),
        "SIBLING-SECRET-9",
    )
    .await;
    let output = sh(
        &fixture,
        &format!("echo x > {}", quoted(&sibling.join("a.txt"))),
        SandboxMode::WorkspaceWrite,
        false,
    )
    .await;
    assert_ne!(output.exit_code, Some(0), "wrote the sibling worktree");
    assert_eq!(
        std::fs::read_to_string(sibling.join("a.txt")).expect("a"),
        "a\n"
    );
}

/// #1420: what is denied inside a task worktree (a `path_deny` match, a
/// deny-list file) stays unreadable although the worktree, which lives in
/// the denied data, is opened again: for the worktree's own commands (its
/// writable root) and for host git (a readable root).
#[tokio::test]
async fn denied_files_inside_an_opened_worktree_stay_unreadable() {
    if !available() {
        return;
    }
    let (mut fixture, _, _) = worktree_fixture();
    let own = fixture.workspace.root().to_path_buf();
    fixture.workspace = Workspace::open(&own, &[".env".to_owned()]).expect("open");
    fixture.config.deny_read.push(own.join("secret.txt"));
    plant(&own.join(".env"), "ENV-SECRET-3");
    plant(&own.join("secret.txt"), "LISTED-SECRET-4");
    let readable = sh(&fixture, "cat a.txt", SandboxMode::WorkspaceWrite, false).await;
    assert_eq!(readable.exit_code, Some(0), "{}", readable.stderr);
    assert_refused(&fixture, "cat .env", "ENV-SECRET-3").await;
    assert_refused(&fixture, "cat secret.txt", "LISTED-SECRET-4").await;

    // Host git's view: the worktree a readable root inside the denied data.
    let data = own.parent().and_then(Path::parent).expect("data");
    let request = elitea_local_tools::sandbox::SandboxRequest {
        deny_paths: vec![data.to_path_buf(), own.join("secret.txt")],
        readable_roots: vec![own.clone()],
        ..elitea_local_tools::sandbox::SandboxRequest::new(SandboxMode::ReadOnly, false)
    };
    let cat = |file: &str| {
        let prepared = elitea_local_tools::sandbox::prepare(
            &request,
            &["/bin/cat".to_owned(), own.join(file).display().to_string()],
            &elitea_local_tools::sandbox::SandboxConfig::default(),
        )
        .expect("prepare");
        std::process::Command::new(&prepared.program)
            .args(&prepared.args)
            .output()
            .expect("run")
    };
    let opened = cat("a.txt");
    assert!(opened.status.success(), "{opened:?}");
    let denied = cat("secret.txt");
    assert!(!denied.status.success(), "read the denied file");
    assert!(!String::from_utf8_lossy(&denied.stdout).contains("LISTED-SECRET-4"));
}

/// A hook (#1237) runs under the same sandbox as a command: it cannot read
/// the credentials under the home or the session's data, whatever its
/// sandbox mode, and its environment is built from nothing.
#[tokio::test]
async fn hook_process_cannot_read_credentials() {
    use elitea_local_tools::hooks::{
        Hook, HookCall, HookEnv, HookEvent, HookSet, RunResult, Scope,
    };
    use elitea_local_tools::policy::LocalWorkPolicy;

    if !available() {
        return;
    }
    let fixture = fixture();
    plant(&fixture.home.join(".ssh/id_ed25519"), "hook-secret-key");
    let probe = format!(
        "cat {}/.ssh/id_ed25519; echo exit=$?; env",
        quoted(&fixture.home)
    );
    let mut hook = Hook::new(
        "probe",
        HookEvent::Stop,
        vec!["/bin/sh".to_owned(), "-c".to_owned(), probe],
    );
    hook.sandbox = SandboxMode::WorkspaceWrite;
    let hooks = HookSet::new();
    hooks.configure(vec![(Scope::User, hook)]);
    let policy = LocalWorkPolicy {
        allowed: true,
        shell: true,
        max_sandbox_mode: SandboxMode::WorkspaceWrite,
        ..LocalWorkPolicy::default()
    };
    let env = HookEnv {
        workspace: &fixture.workspace,
        shell: &fixture.config,
        policy: &policy,
        session_id: "s1",
        conversation: None,
    };
    let outcome = hooks.run(&env, HookCall::event(HookEvent::Stop)).await;
    assert_eq!(outcome.runs[0].result, RunResult::Annotated, "{outcome:?}");
    let text = &outcome.annotations[0].1;
    assert!(!text.contains("hook-secret-key"), "{text}");
    assert!(!text.contains("exit=0"), "{text}");
    assert!(text.contains("ELITEA_HOOK_ID=probe"), "{text}");
    assert!(!text.contains("CARGO_MANIFEST_DIR"), "{text}");
}

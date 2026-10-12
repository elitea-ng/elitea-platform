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
    let output = run(
        &fixture.workspace,
        &fixture.config,
        &CommandSpec {
            command: command.to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode,
            network,
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

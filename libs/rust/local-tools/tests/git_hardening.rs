//! The host runs git in folders the agent (or whoever wrote the repository)
//! controls. Each test plants one way repository-controlled code used to
//! run outside the sandbox through the host's own git calls — filter
//! drivers, gpg.program, core.worktree, includes, a relocated `.git`,
//! commondir, submodules — and proves none of it runs: the repository is
//! refused with `local_tools.unsafe_repository`, checkpoints fall back to
//! copies, and the marker the payload would write never appears.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;

use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::checkpoint::Checkpoints;
use elitea_local_tools::deny::DenyList;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::git::{
    RegisteredWorktree, Repo, forget_worktree_parent, trust_worktree_parent,
};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::sandbox::SandboxConfig;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::workspace::Workspace;
use elitea_local_tools::worktree;
use serde_json::{Value, json};

fn git(dir: &Path, args: &[&str]) {
    let output = Command::new("git")
        .current_dir(dir)
        .args([
            "-c",
            "user.name=T",
            "-c",
            "user.email=t@example.com",
            "-c",
            "commit.gpgsign=false",
            "-c",
            "init.defaultBranch=main",
            "-c",
            "core.hooksPath=/dev/null",
        ])
        .args(args)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .output()
        .expect("git");
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
}

struct Planted {
    _dir: tempfile::TempDir,
    root: PathBuf,
    outside: PathBuf,
    marker: PathBuf,
    data: PathBuf,
}

/// A committed repository, an outside directory, and the marker path a
/// payload would create.
fn repository() -> Planted {
    let dir = tempfile::tempdir().expect("dir");
    let base = std::fs::canonicalize(dir.path()).expect("canonical");
    let root = base.join("repo");
    let outside = base.join("outside");
    std::fs::create_dir_all(&root).expect("repo");
    std::fs::create_dir_all(&outside).expect("outside");
    git(&root, &["init", "-q"]);
    std::fs::write(root.join("a.txt"), "a\n").expect("a");
    git(&root, &["add", "-A"]);
    git(&root, &["commit", "-q", "-m", "init"]);
    std::fs::write(root.join("a.txt"), "changed\n").expect("change");
    Planted {
        marker: outside.join("pwned"),
        data: base.join("data"),
        _dir: dir,
        root,
        outside,
    }
}

fn payload(marker: &Path) -> String {
    // Config values: `;` and `#` start comments, quotes are stripped.
    format!("touch {}", marker.display())
}

fn append(path: &Path, text: &str) {
    let mut current = std::fs::read_to_string(path).unwrap_or_default();
    current.push_str(text);
    std::fs::write(path, current).expect("append");
}

/// Every host path that runs git: opening checkpoints, taking one,
/// restoring one, and the git tools. None may run the payload.
async fn exercise(planted: &Planted) -> (Checkpoints, Value) {
    exercise_at(&planted.root, planted).await
}

/// [`exercise`] in the folder `root` (a task worktree of the planted
/// repository).
async fn exercise_at(root: &Path, planted: &Planted) -> (Checkpoints, Value) {
    let workspace = Workspace::open(root, &[]).expect("workspace");
    let checkpoints = Checkpoints::open(&workspace, "s1", &planted.data).expect("open");
    if let Ok(info) = checkpoints.create("turn") {
        let _ = checkpoints.restore(info.seq);
    }
    let session = LocalSession::open(SessionConfig {
        spill_dir: None, // under data_dir
        root: root.to_path_buf(),
        session_id: "s1".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(DeferringPrompt),
        data_dir: planted.data.clone(),
        shell: None,
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
        documents: elitea_local_tools::document::Documents::default(),
    })
    .expect("session");
    let mut results = Vec::new();
    for tool in ["git_status", "git_diff", "git_log", "git_branches"] {
        results.push(session.call(tool, "call-1", json!({})).await);
    }
    assert!(
        !planted.marker.exists(),
        "repository-controlled code ran on the host"
    );
    (checkpoints, Value::Array(results))
}

fn assert_refused(checkpoints: &Checkpoints, results: &Value) {
    assert_eq!(
        checkpoints.kind(),
        "copy",
        "an unsafe repository is not used"
    );
    let refusal = checkpoints.git_refusal().expect("a refusal");
    assert_eq!(refusal.code(), ErrorCode::UnsafeRepository);
    for result in results.as_array().expect("results") {
        assert_eq!(result["code"], "local_tools.unsafe_repository", "{result}");
    }
}

#[tokio::test]
async fn a_safe_repository_still_checkpoints_through_git() {
    let planted = repository();
    let (checkpoints, results) = exercise(&planted).await;
    assert_eq!(checkpoints.kind(), "git");
    assert!(checkpoints.git_refusal().is_none());
    for result in results.as_array().expect("results") {
        assert_eq!(result["status"], "ok", "{result}");
    }
}

#[tokio::test]
async fn filter_drivers_in_the_repository_config_never_run() {
    let planted = repository();
    let command = payload(&planted.marker);
    append(
        &planted.root.join(".git/config"),
        &format!("[filter \"pwn\"]\n\tclean = {command} && cat\n\tsmudge = {command} && cat\n"),
    );
    std::fs::write(planted.root.join(".gitattributes"), "* filter=pwn\n").expect("attrs");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn gpg_program_and_signing_never_run() {
    let planted = repository();
    let script = planted.outside.join("gpg.sh");
    std::fs::write(
        &script,
        format!("#!/bin/sh\n{}\n", payload(&planted.marker)),
    )
    .expect("gpg");
    Command::new("chmod")
        .arg("+x")
        .arg(&script)
        .status()
        .expect("chmod");
    append(
        &planted.root.join(".git/config"),
        &format!(
            "[commit]\n\tgpgSign = true\n[log]\n\tshowSignature = true\n[gpg]\n\tprogram = {}\n",
            script.display()
        ),
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn a_core_worktree_elsewhere_is_refused() {
    let planted = repository();
    append(
        &planted.root.join(".git/config"),
        &format!("[core]\n\tworktree = {}\n", planted.outside.display()),
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
    assert_eq!(
        std::fs::read_dir(&planted.outside)
            .expect("outside")
            .count(),
        0,
        "nothing was written to the other work tree"
    );
}

#[tokio::test]
async fn includes_aliases_and_fsmonitor_in_the_config_are_refused() {
    for snippet in [
        "[include]\n\tpath = ../evil.cfg\n",
        "[includeIf \"gitdir:/\"]\n\tpath = ../evil.cfg\n",
        "[alias]\n\tst = !touch x\n",
        "[core]\n\tfsmonitor = touch x\n",
        "[core]\n\tsshCommand = touch x\n",
        "[diff \"x\"]\n\ttextconv = touch x\n",
        "[merge \"x\"]\n\tdriver = touch x\n",
        "[credential]\n\thelper = touch x\n",
        "[submodule \"m\"]\n\tupdate = !touch x\n",
        "[url \"x\"]\n\tinsteadOf = y\n",
        "[core]\n\tpager = touch x\n",
        "[this is not a config line\n",
    ] {
        let planted = repository();
        std::fs::write(
            planted.root.join("evil.cfg"),
            format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
        )
        .expect("evil");
        append(&planted.root.join(".git/config"), snippet);
        let (checkpoints, results) = exercise(&planted).await;
        assert_refused(&checkpoints, &results);
    }
}

#[tokio::test]
async fn a_relocated_git_dir_is_refused() {
    let planted = repository();
    let elsewhere = planted.outside.join("real.git");
    std::fs::rename(planted.root.join(".git"), &elsewhere).expect("move");
    append(
        &elsewhere.join("config"),
        &format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    );
    std::fs::write(
        planted.root.join(".git"),
        format!("gitdir: {}\n", elsewhere.display()),
    )
    .expect("gitdir file");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn commondir_and_submodule_git_dirs_are_refused() {
    let planted = repository();
    std::fs::write(
        planted.root.join(".git/commondir"),
        planted.outside.display().to_string(),
    )
    .expect("commondir");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);

    // A submodule git directory with a gitlink that uses it: refused.
    let planted = repository();
    let module = planted.root.join(".git/modules/m");
    std::fs::create_dir_all(&module).expect("module");
    std::fs::write(
        module.join("config"),
        format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    )
    .expect("module config");
    git(
        &planted.root,
        &[
            "update-index",
            "--add",
            "--cacheinfo",
            &format!("160000,{},m", "1".repeat(40)),
        ],
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
}

/// A git directory left behind by a submodule since removed (no gitlink,
/// no `.gitmodules`), as git writes it, with its `[submodule]` section:
/// accepted.
#[tokio::test]
async fn a_stale_submodule_git_dir_is_accepted() {
    let planted = repository();
    let module = planted.root.join(".git/modules/m");
    std::fs::create_dir_all(&module).expect("module");
    std::fs::write(
        module.join("config"),
        "[core]\n\tbare = false\n\tworktree = ../../../m\n",
    )
    .expect("module config");
    append(
        &planted.root.join(".git/config"),
        "[submodule \"m\"]\n\turl = ../m\n\tactive = true\n",
    );
    let (checkpoints, results) = exercise(&planted).await;
    assert_eq!(checkpoints.kind(), "git", "{:?}", checkpoints.git_refusal());
    for result in results.as_array().expect("results") {
        assert_eq!(result["status"], "ok", "{result}");
    }
}

/// A stale submodule git directory whose config carries an fsmonitor
/// payload, and a work-tree `.git` file pointing into it: the agent's tools
/// cannot write that file (`.git` is protected at any depth), the
/// repository is refused for the module's config, and the payload never
/// runs through any host git path.
#[tokio::test]
async fn a_gitfile_into_a_stale_module_with_a_payload_never_runs_it() {
    let planted = repository();
    let module = planted.root.join(".git/modules/m");
    std::fs::create_dir_all(&module).expect("module");
    std::fs::write(
        module.join("config"),
        format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    )
    .expect("module config");
    let workspace = Workspace::open(&planted.root, &[]).expect("workspace");
    let gitfile =
        elitea_local_tools::workspace::WsPath::from_relative(Path::new("m/.git")).expect("path");
    assert!(
        workspace
            .write(&gitfile, b"gitdir: ../.git/modules/m\n", None)
            .is_err(),
        "the agent's tools must not write a .git file"
    );
    // Planted from outside the agent's reach (the person's own folder).
    std::fs::create_dir_all(planted.root.join("m")).expect("m");
    std::fs::write(planted.root.join("m/.git"), "gitdir: ../.git/modules/m\n").expect("gitfile");
    std::fs::write(planted.root.join("m/f.txt"), "f\n").expect("file");
    let (checkpoints, results) = exercise(&planted).await;
    assert_refused(&checkpoints, &results);
    assert!(!planted.marker.exists());
}

/// `core.hooksPath` is accepted, and no hook in it (or in `.git/hooks`)
/// ever runs through the host's git: checkpoints, a restore, the git
/// tools, and making a task worktree (`worktree add`, `read-tree`,
/// `checkout-index`, ref updates) and using it.
#[tokio::test]
async fn a_hooks_path_is_accepted_and_its_hooks_never_run() {
    for hooks_path in ["hooks", ".git/hooks"] {
        let planted = repository();
        let hooks = planted.root.join(hooks_path);
        std::fs::create_dir_all(&hooks).expect("hooks");
        for hook in [
            "reference-transaction",
            "post-checkout",
            "post-index-change",
            "pre-commit",
            "post-commit",
            "pre-auto-gc",
        ] {
            let path = hooks.join(hook);
            std::fs::write(&path, format!("#!/bin/sh\n{}\n", payload(&planted.marker)))
                .expect("hook");
            std::fs::set_permissions(&path, std::os::unix::fs::PermissionsExt::from_mode(0o755))
                .expect("chmod");
        }
        append(
            &planted.root.join(".git/config"),
            &format!("[core]\n\thooksPath = {hooks_path}\n"),
        );
        let (checkpoints, results) = exercise(&planted).await;
        assert_eq!(checkpoints.kind(), "git", "{:?}", checkpoints.git_refusal());
        for result in results.as_array().expect("results") {
            assert_eq!(result["status"], "ok", "{result}");
        }
        let entry = task_worktree(&planted, "hooks1");
        let (checkpoints, _) = exercise_at(&entry.worktree, &planted).await;
        assert_eq!(checkpoints.kind(), "git");
        assert!(!planted.marker.exists(), "a hook ran ({hooks_path})");
    }
}

/// A task worktree of the planted repository, created by the host under
/// `<data>/worktrees/<name>` and registered.
fn task_worktree(planted: &Planted, name: &str) -> RegisteredWorktree {
    let repo = Repo::discover(
        &planted.root,
        &SandboxConfig::default(),
        Arc::new(DenyList::new([])),
    )
    .expect("discover")
    .expect("a repository");
    let dir = planted.data.join("worktrees");
    std::fs::create_dir_all(&dir).expect("worktrees dir");
    let created = worktree::create(
        &repo,
        &dir.join(name),
        name,
        &worktree::branch_name(name),
        "HEAD",
    )
    .expect("create");
    created.entry
}

fn open(entry: &RegisteredWorktree) -> Result<Repo, elitea_local_tools::error::ToolError> {
    Repo::open_registered_worktree(
        entry,
        &SandboxConfig::default(),
        Arc::new(DenyList::new([])),
    )
}

#[tokio::test]
async fn registered_worktree_is_accepted() {
    let planted = repository();
    let entry = task_worktree(&planted, "a1b2c3d4");
    let repo = Repo::discover(
        &entry.worktree,
        &SandboxConfig::default(),
        Arc::new(DenyList::new([])),
    )
    .expect("accepted")
    .expect("a repository");
    assert!(repo.is_linked_worktree());
    assert_eq!(repo.registered(), Some(&entry));
    assert_eq!(
        repo.git_dir(),
        planted.root.join(".git/worktrees/a1b2c3d4").as_path()
    );
    // The base's files, not the folder's uncommitted change.
    assert_eq!(
        std::fs::read_to_string(entry.worktree.join("a.txt")).expect("a"),
        "a\n"
    );
    let (checkpoints, results) = exercise_at(&entry.worktree, &planted).await;
    assert_eq!(checkpoints.kind(), "git");
    for result in results.as_array().expect("results") {
        assert_eq!(result["status"], "ok", "{result}");
    }
}

#[tokio::test]
async fn unregistered_gitdir_file_is_refused() {
    let planted = repository();
    // A worktree git itself made: the repository declares it, the host
    // never registered it.
    let other = planted.outside.join("wt");
    git(
        &planted.root,
        &[
            "worktree",
            "add",
            "-q",
            "--detach",
            &other.to_string_lossy(),
        ],
    );
    let refused = Repo::discover(
        &other,
        &SandboxConfig::default(),
        Arc::new(DenyList::new([])),
    )
    .expect_err("refused");
    assert_eq!(refused.code(), ErrorCode::UnsafeRepository);
    let (checkpoints, results) = exercise_at(&other, &planted).await;
    assert_refused(&checkpoints, &results);
    // Naming it does not help either: it is not where the host puts one.
    let named = RegisteredWorktree {
        worktree: other.clone(),
        top: planted.root.clone(),
        name: "elsewhere".to_owned(),
    };
    assert_eq!(
        open(&named).expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
}

/// A worktree the person made with `git worktree add` on another branch
/// and opened on its own (its repository is not an open workspace): host
/// git and git checkpoints work in it, so turns on two branches run in
/// parallel. The repository's `.git` is still checked: a dangerous key in
/// its config refuses the worktree as well.
#[tokio::test]
async fn an_opened_worktree_of_its_own_is_accepted() {
    let planted = repository();
    let own = planted.outside.join("feature");
    git(
        &planted.root,
        &[
            "worktree",
            "add",
            "-q",
            "-b",
            "feature",
            &own.to_string_lossy(),
        ],
    );
    let discover = || Repo::discover(&own, &SandboxConfig::default(), Arc::new(DenyList::new([])));
    assert_eq!(
        discover().expect_err("not open yet").code(),
        ErrorCode::UnsafeRepository
    );
    trust_worktree_parent(&own);
    let repo = discover().expect("accepted").expect("a repository");
    assert!(repo.is_linked_worktree());
    assert_eq!(
        repo.common_dir(),
        std::fs::canonicalize(planted.root.join(".git")).expect("common")
    );
    let workspace = Workspace::open(&own, &[]).expect("workspace");
    let checkpoints = Checkpoints::open(&workspace, "own1", &planted.data).expect("open");
    assert!(
        checkpoints.git_refusal().is_none(),
        "{:?}",
        checkpoints.git_refusal()
    );
    assert_eq!(checkpoints.kind(), "git");
    // The repository's own config is still read and checked.
    append(
        &planted.root.join(".git/config"),
        &format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    );
    assert_eq!(
        discover().expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
    let (checkpoints, results) = exercise_at(&own, &planted).await;
    assert_refused(&checkpoints, &results);
    forget_worktree_parent(&own);
}

#[tokio::test]
async fn gitdir_pointing_outside_top_repo_is_refused() {
    let planted = repository();
    let entry = task_worktree(&planted, "feedc0de");
    let foreign = repository();
    let foreign_wt = foreign.outside.join("wt");
    git(
        &foreign.root,
        &[
            "worktree",
            "add",
            "-q",
            "--detach",
            &foreign_wt.to_string_lossy(),
        ],
    );
    let foreign_admin =
        std::fs::canonicalize(foreign.root.join(".git/worktrees/wt")).expect("admin");
    std::fs::write(
        entry.worktree.join(".git"),
        format!("gitdir: {}\n", foreign_admin.display()),
    )
    .expect("rewrite");
    assert_eq!(
        open(&entry).expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
    let (checkpoints, results) = exercise_at(&entry.worktree, &planted).await;
    assert_refused(&checkpoints, &results);
    // A relative pointer, even to the right place, is not what the host wrote.
    std::fs::write(
        entry.worktree.join(".git"),
        "gitdir: ../../../repo/.git/worktrees/feedc0de\n",
    )
    .expect("rewrite");
    assert_eq!(
        open(&entry).expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
}

#[tokio::test]
async fn commondir_elsewhere_is_refused() {
    let planted = repository();
    let entry = task_worktree(&planted, "c0mm0nd1");
    let foreign = repository();
    let admin = planted.root.join(".git/worktrees/c0mm0nd1");
    std::fs::write(
        admin.join("commondir"),
        format!("{}\n", foreign.root.join(".git").display()),
    )
    .expect("commondir");
    assert_eq!(
        open(&entry).expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
    let (checkpoints, results) = exercise_at(&entry.worktree, &planted).await;
    assert_refused(&checkpoints, &results);
}

#[tokio::test]
async fn worktree_config_with_dangerous_key_is_refused() {
    let planted = repository();
    let entry = task_worktree(&planted, "c0nf1g00");
    std::fs::write(
        planted.root.join(".git/worktrees/c0nf1g00/config.worktree"),
        format!("[core]\n\tfsmonitor = {}\n", payload(&planted.marker)),
    )
    .expect("config.worktree");
    assert_eq!(
        open(&entry).expect_err("refused").code(),
        ErrorCode::UnsafeRepository
    );
    let (checkpoints, results) = exercise_at(&entry.worktree, &planted).await;
    assert_refused(&checkpoints, &results);
}

/// #1420: a branch a removed worktree kept (its commits) does not block a
/// new worktree of the same thread: the new one gets the first free
/// `-<n>` suffix, and 12 letters of the id name the branch.
#[tokio::test]
async fn a_kept_branch_gets_a_suffixed_successor() {
    let planted = repository();
    let name = "0123456789abcdef0123456789abcdef";
    assert_eq!(worktree::branch_name(name), "elitea/0123456789ab");
    let repo = Repo::discover(
        &planted.root,
        &SandboxConfig::default(),
        Arc::new(DenyList::new([])),
    )
    .expect("discover")
    .expect("a repository");
    let dir = planted.data.join("worktrees");
    std::fs::create_dir_all(&dir).expect("worktrees dir");
    let branch = worktree::branch_name(name);
    let first = worktree::create(&repo, &dir.join(name), name, &branch, "HEAD").expect("create");
    assert_eq!(first.branch, branch);
    worktree::remove(&repo, &first.entry, true).expect("remove, branch kept");
    let second = worktree::create(&repo, &dir.join(name), name, &branch, "HEAD").expect("again");
    assert_eq!(second.branch, format!("{branch}-2"));
    let listed = String::from_utf8(
        std::process::Command::new("git")
            .current_dir(&planted.root)
            .args(["branch", "--list", "elitea/*"])
            .output()
            .expect("git")
            .stdout,
    )
    .expect("utf-8");
    assert!(listed.contains("elitea/0123456789ab\n"), "{listed}");
    assert!(listed.contains("elitea/0123456789ab-2"), "{listed}");
}

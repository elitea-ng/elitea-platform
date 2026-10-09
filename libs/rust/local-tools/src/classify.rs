//! Which commands are destructive enough to ask about: the default rule
//! for `run_command` (ADR-0029 decision 4; the owner's decision "do not ask
//! for approval if it's not very destructive").
//!
//! A command that runs in a confining sandbox (`read-only` or
//! `workspace-write`, no network) and that this module calls
//! [`Risk::Routine`] runs without a question: the sandbox keeps its writes
//! in the workspace and the turn's checkpoint can undo them. A command is
//! [`Risk::Destructive`] (asked) when any segment of it, at any depth this
//! analysis reaches, is one of these:
//!
//! | Class | Asked | Not asked |
//! |-------|-------|-----------|
//! | Deletes | `rm -r`/`-R`/`--recursive`, `rm` with a glob, more than 10 paths or none (`xargs rm`), `find … -delete`, `find … -exec rm`, `rmdir -p`, `git clean` (not `-n`), `shred`, `truncate` | `rm file.txt`, `rmdir dir`, `unlink f` |
//! | Git history and discards | `push` (any), `reset --hard`/`--merge`, `checkout -- <paths>`, `checkout .`, `checkout -f`, `restore` of the working tree, `switch -f`/`--discard-changes`, `rebase`, `filter-branch`, `filter-repo`, `branch -D`/`-M`/`-f`, `tag -d`/`-f`, `stash drop`/`clear`, `reflog expire`/`delete`, `gc --prune…`, `prune`, `update-ref -d`, `-c alias.…` | `status`, `diff`, `log`, `add`, `commit`, `fetch`, `checkout -b`, `restore --staged`, `stash`, `branch -d` |
//! | Publish and deploy | `npm`/`pnpm`/`yarn`/`bun publish` (and `unpublish`, `deprecate`, `dist-tag`, `owner`, `access`), `cargo publish`/`yank`/`owner`, `twine upload`, `uv publish`, `poetry publish`, `gem push`/`yank`, `docker`/`podman` other than read verbs and `build`, `kubectl`/`oc`, `helm`, `terraform`/`tofu`/`terragrunt`, `pulumi` other than read and plan verbs, `gh` other than read verbs, every `aws`, `gcloud`, `gsutil`, `az`, `azd`, `doctl`, `fly`, `vercel`, `netlify`, `heroku`, `firebase`, `wrangler`, `eksctl`, `rclone`, `s3cmd`, `oci` | `kubectl get`, `helm template`, `terraform plan`, `pulumi preview`, `gh pr view`, `docker build` |
//! | Dependencies | `npm`/`pnpm`/`yarn`/`bun install`/`i`/`add`/`update` (bare `yarn` too) or `-g`, `cargo install`/`add`/`uninstall`, `pip install`/`uninstall`, `uv add`/`remove`/`sync`/`pip install`/`tool install`, `poetry add`/`remove`/`install`/`update`, `pipx`, `gem install`, `go install`/`get`, `brew`/`apt`/`dnf`/… other than read verbs | `npm ci`, `npm test`, `cargo build`, `go build` |
//! | Privilege and system | `sudo`, `su`, `doas`, `pkexec`, `chmod`/`chown`/`chgrp -R`, `dd`, `mkfs*`, `newfs*`, `diskutil`, `fdisk`, `parted`, `wipefs`, `mount`, `umount`, `kill -9`/`-KILL`/`-1`, `killall`, `pkill`, `launchctl`, `systemctl`, `service`, `shutdown`, `reboot`, `crontab` other than `-l`, `osascript` | `kill 1234`, `chmod +x script.sh` |
//! | Shell constructs | `sh -c`/`bash -c`/`eval` whose script is destructive or cannot be parsed, a destructive command in `$(…)`, `` `…` `` or `<(…)`, a download (`curl`, `wget`) together with a shell or interpreter (`curl … \| sh`, `bash <(curl …)`), a redirect or `tee` writing outside the workspace (`echo hi > /etc/x`), unbalanced quotes | `cargo test 2>&1 \| tee target/log`, `npm ci && npm test`, `ls > out.txt` |
//!
//! The analysis reads what the model wrote; like `command_deny`, it cannot
//! see what a script, a Makefile or an interpreter runs. It decides only
//! whether to *ask*: the sandbox is what confines a command, and the
//! policy, workspace rules and remembered choices still apply on top
//! (see [`crate::approvals`]).

use std::path::{Component, Path, PathBuf};

use crate::command::{CommandShape, SHELLS, analyse, basename, needs_shell};

/// What the default rule makes of a command.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Risk {
    /// Runs without a question in a confining sandbox without network.
    Routine,
    /// Asked; the reason.
    Destructive(String),
}

impl Risk {
    #[must_use]
    pub fn is_routine(&self) -> bool {
        matches!(self, Self::Routine)
    }
}

/// Where a command runs: the workspace root and its working directory
/// (absolute).
#[derive(Clone, Copy, Debug)]
pub struct Place<'a> {
    pub root: &'a Path,
    pub cwd: &'a Path,
}

/// Classify one command string.
#[must_use]
pub fn assess(command: &str, place: Place<'_>) -> Risk {
    assess_at(command, place, 0).map_or(Risk::Routine, Risk::Destructive)
}

/// Deepest nesting of `sh -c` / `eval` read.
const MAX_DEPTH: usize = 4;

/// More operands than this make an `rm` a bulk delete.
const BULK_PATHS: usize = 10;

const DELETERS: &[&str] = &["rm", "rmdir", "unlink", "shred", "truncate", "srm"];

const FETCHERS: &[&str] = &["curl", "wget", "fetch", "http", "https", "aria2c"];

/// What runs the text it is fed (with [`SHELLS`]).
const RUNNERS: &[&str] = &[
    "eval", "source", ".", "python", "python3", "node", "perl", "ruby", "php", "deno",
];

const PRIVILEGE: &[&str] = &["sudo", "sudoedit", "su", "doas", "pkexec", "runas"];

const SYSTEM: &[&str] = &[
    "dd",
    "diskutil",
    "fdisk",
    "sfdisk",
    "gdisk",
    "parted",
    "wipefs",
    "mount",
    "umount",
    "launchctl",
    "systemctl",
    "service",
    "shutdown",
    "reboot",
    "halt",
    "poweroff",
    "killall",
    "pkill",
    "osascript",
];

const CLOUD: &[&str] = &[
    "aws", "gcloud", "gsutil", "az", "azd", "doctl", "flyctl", "fly", "vercel", "netlify",
    "heroku", "firebase", "wrangler", "eksctl", "rclone", "s3cmd", "oci", "ibmcloud",
];

const SYSTEM_PACKAGES: &[&str] = &[
    "brew", "port", "apt", "apt-get", "yum", "dnf", "pacman", "snap", "zypper", "apk", "nix-env",
];

/// An "ask" verdict (`None` everywhere here means routine).
#[allow(clippy::unnecessary_wraps)]
fn ask(reason: impl Into<String>) -> Option<String> {
    Some(reason.into())
}

fn assess_at(text: &str, place: Place<'_>, depth: usize) -> Option<String> {
    if depth > MAX_DEPTH {
        return ask("nested shells too deep to read");
    }
    let shape = analyse(text);
    if let CommandShape::Compound { reason, .. } = &shape
        && (*reason == "unbalanced quotes" || *reason == "not a parsable command")
    {
        return ask(format!("the command cannot be read ({reason})"));
    }
    let segments = shape.segments();
    let compound = needs_shell(text).is_some();
    let mut targets: Vec<Option<String>> = if compound {
        redirect_targets(text)
    } else {
        Vec::new()
    };
    for argv in segments.iter().filter(|argv| basename(&argv[0]) == "tee") {
        targets.extend(
            operands(&argv[1..])
                .into_iter()
                .map(|path| Some(path.to_owned())),
        );
    }
    // A `cd` elsewhere moves where relative targets land.
    let moved = compound
        && !targets.is_empty()
        && segments.iter().any(|argv| {
            matches!(basename(&argv[0]), "cd" | "pushd")
                && argv.get(1).is_none_or(|dir| !inside(dir, place))
        });
    if moved {
        return ask("writes after changing to a directory outside the workspace");
    }
    if let Some(target) = targets
        .iter()
        .find(|target| target.as_deref().is_none_or(|path| !inside(path, place)))
    {
        return ask(match target {
            Some(path) => format!("writes `{path}`, outside the workspace"),
            None => "writes to a path only the shell can resolve".to_owned(),
        });
    }
    for argv in &segments {
        if let Some(reason) = argv_risk(argv, place, depth) {
            return Some(reason);
        }
    }
    let program = |list: &[&str]| {
        segments
            .iter()
            .any(|argv| list.contains(&basename(&argv[0])))
    };
    if program(FETCHERS) && (program(SHELLS) || program(RUNNERS)) {
        return ask("runs downloaded content");
    }
    None
}

/// Whether `path` (relative to the working directory) stays inside the
/// workspace, lexically.
fn inside(path: &str, place: Place<'_>) -> bool {
    if matches!(
        path,
        "/dev/null" | "/dev/stdout" | "/dev/stderr" | "/dev/tty"
    ) || path.starts_with("/dev/fd/")
    {
        return true;
    }
    if path.is_empty() || path.starts_with('~') || path.contains(['*', '?', '[', '$', '`']) {
        return false;
    }
    let joined = place.cwd.join(path);
    let mut normal = PathBuf::new();
    for component in joined.components() {
        match component {
            Component::ParentDir => {
                if !normal.pop() {
                    return false;
                }
            }
            Component::CurDir => {}
            other => normal.push(other),
        }
    }
    normal.starts_with(place.root)
}

/// The targets of the output redirections in `text` (`None`: a target only
/// the shell can resolve). Descriptor duplications (`2>&1`) and process
/// substitutions (`>(…)`) are not targets.
fn redirect_targets(text: &str) -> Vec<Option<String>> {
    let chars: Vec<char> = text.chars().collect();
    let mut out = Vec::new();
    let (mut single, mut double) = (false, false);
    let mut index = 0;
    while index < chars.len() {
        let character = chars[index];
        index += 1;
        if single {
            single = character != '\'';
            continue;
        }
        if double {
            match character {
                '"' => double = false,
                '\\' => index += 1,
                _ => {}
            }
            continue;
        }
        match character {
            '\'' => single = true,
            '"' => double = true,
            '\\' => index += 1,
            '>' => {
                if chars.get(index) == Some(&'(') {
                    continue;
                }
                if matches!(chars.get(index), Some('>' | '|')) {
                    index += 1;
                }
                if chars.get(index) == Some(&'&') {
                    index += 1;
                    if chars
                        .get(index)
                        .is_some_and(|next| next.is_ascii_digit() || *next == '-')
                    {
                        continue;
                    }
                }
                while chars
                    .get(index)
                    .is_some_and(|next| matches!(next, ' ' | '\t'))
                {
                    index += 1;
                }
                let (word, next) = read_word(&chars, index);
                out.push(word);
                index = next;
            }
            _ => {}
        }
    }
    out
}

/// One shell word from `start`, quotes removed; `None` when it expands.
fn read_word(chars: &[char], start: usize) -> (Option<String>, usize) {
    let mut word = String::new();
    let mut expands = false;
    let (mut single, mut double) = (false, false);
    let mut index = start;
    while let Some(&character) = chars.get(index) {
        if single {
            if character == '\'' {
                single = false;
            } else {
                word.push(character);
            }
            index += 1;
            continue;
        }
        match character {
            '\'' if !double => single = true,
            '"' => double = !double,
            '$' | '`' => {
                expands = true;
                word.push(character);
            }
            ' ' | '\t' | '\n' | '|' | '&' | ';' | '<' | '>' | '(' | ')' if !double => break,
            _ => word.push(character),
        }
        index += 1;
    }
    let word = (!expands && !word.is_empty()).then_some(word);
    (word, index)
}

/// Short option clusters (`-rf`) before `--`.
fn short_flags(args: &[String]) -> impl Iterator<Item = &str> {
    args.iter()
        .take_while(|word| *word != "--")
        .map(String::as_str)
        .filter(|word| word.starts_with('-') && !word.starts_with("--") && word.len() > 1)
}

fn has_short(args: &[String], flag: char) -> bool {
    short_flags(args).any(|word| word[1..].contains(flag))
}

fn has_long(args: &[String], flag: &str) -> bool {
    args.iter()
        .take_while(|word| *word != "--")
        .any(|word| word == flag || word.starts_with(&format!("{flag}=")))
}

/// The words that are not options (`-x`, `--x`).
fn operands(args: &[String]) -> Vec<&str> {
    let mut out = Vec::new();
    let mut literal = false;
    for word in args {
        if !literal && word == "--" {
            literal = true;
        } else if literal || !word.starts_with('-') || word == "-" {
            out.push(word.as_str());
        }
    }
    out
}

/// The first word that is not an option.
fn subcommand(args: &[String]) -> Option<&str> {
    args.iter()
        .map(String::as_str)
        .find(|word| !word.starts_with('-'))
}

/// One segment's risk.
fn argv_risk(segment: &[String], place: Place<'_>, depth: usize) -> Option<String> {
    let (word, args) = segment.split_first()?;
    let program = basename(word);
    let named = |what: &str| format!("`{program}` {what}");
    if PRIVILEGE.contains(&program) {
        return ask(named("runs as another user"));
    }
    if SYSTEM.contains(&program) || program.starts_with("mkfs") || program.starts_with("newfs") {
        return ask(named("changes the system, disks or other processes"));
    }
    if CLOUD.contains(&program) {
        return ask(named("is a cloud CLI (asked by default)"));
    }
    if SHELLS.contains(&program) {
        let script = args
            .iter()
            .position(|word| word.starts_with('-') && !word.starts_with("--") && word.contains('c'))
            .and_then(|flag| args.get(flag + 1));
        return script.and_then(|script| {
            assess_at(script, place, depth + 1).map(|reason| format!("`{program} -c`: {reason}"))
        });
    }
    match program {
        "eval" => {
            assess_at(&args.join(" "), place, depth + 1).map(|reason| format!("`eval`: {reason}"))
        }
        "rm" => rm_risk(args),
        "shred" | "truncate" | "srm" => ask(named("destroys file contents")),
        "rmdir" => (has_short(args, 'p') || has_long(args, "--parents"))
            .then(|| named("-p removes parent directories")),
        "find" => find_risk(args, place, depth),
        "chmod" | "chown" | "chgrp" => (has_short(args, 'R') || has_long(args, "--recursive"))
            .then(|| named("-R changes a whole tree")),
        "kill" => {
            let strong = args.iter().any(|word| {
                matches!(
                    word.as_str(),
                    "-9" | "-KILL" | "-SIGKILL" | "-1" | "--signal" | "KILL" | "SIGKILL"
                )
            });
            strong.then(|| named("force-kills or signals every process"))
        }
        "crontab" => (!args.iter().all(|word| word == "-l")).then(|| named("changes the crontab")),
        "git" => git_risk(args),
        "npm" | "pnpm" | "yarn" | "bun" | "cnpm" => node_risk(program, args),
        "cargo" => match subcommand(args) {
            Some("publish" | "yank" | "owner" | "login" | "logout") => {
                ask(named("changes a package registry"))
            }
            Some("install" | "uninstall" | "add") => ask(named("installs or changes dependencies")),
            _ => None,
        },
        "pip" | "pip3" => matches!(subcommand(args), Some("install" | "uninstall"))
            .then(|| named("installs or changes packages")),
        "python" | "python3" => {
            let module = args.iter().position(|word| word == "-m")?;
            argv_risk(&args[module + 1..], place, depth)
        }
        "uv" => uv_risk(args),
        "poetry" => match subcommand(args) {
            Some("publish") => ask(named("publishes a package")),
            Some("add" | "remove" | "install" | "update" | "self") => {
                ask(named("installs or changes dependencies"))
            }
            _ => None,
        },
        "pipx" => ask(named("installs packages")),
        "twine" => matches!(subcommand(args), Some("upload" | "register"))
            .then(|| named("publishes a package")),
        "gem" => match subcommand(args) {
            Some("push" | "yank" | "owner" | "signin") => ask(named("changes a package registry")),
            Some("install" | "uninstall" | "update") => ask(named("installs or changes packages")),
            _ => None,
        },
        "go" => matches!(subcommand(args), Some("install" | "get"))
            .then(|| named("installs or changes dependencies")),
        "docker" | "podman" | "nerdctl" => container_risk(program, args),
        _ => deploy_risk(program, args),
    }
}

/// Deploy, cluster, infrastructure and package-manager CLIs: asked unless
/// the verb only reads.
fn deploy_risk(program: &str, args: &[String]) -> Option<String> {
    match program {
        "kubectl" | "oc" => verb_risk(
            program,
            args,
            &[
                "get",
                "describe",
                "logs",
                "explain",
                "version",
                "api-resources",
                "api-versions",
                "cluster-info",
                "top",
                "diff",
                "wait",
                "help",
            ],
            &[
                (
                    "config",
                    &["view", "get-contexts", "current-context", "get-clusters"],
                ),
                ("auth", &["can-i", "whoami"]),
            ],
        ),
        "helm" => verb_risk(
            program,
            args,
            &[
                "list", "ls", "status", "template", "lint", "show", "version", "get", "history",
                "search", "env", "help",
            ],
            &[("repo", &["list", "ls"]), ("dependency", &["list", "ls"])],
        ),
        "terraform" | "tofu" | "terragrunt" => verb_risk(
            program,
            args,
            &[
                "plan",
                "validate",
                "fmt",
                "init",
                "show",
                "output",
                "version",
                "providers",
                "graph",
                "help",
            ],
            &[("state", &["list", "show"])],
        ),
        "pulumi" => verb_risk(
            program,
            args,
            &["preview", "version", "about", "whoami", "logs", "help"],
            &[("stack", &["ls", "output"]), ("config", &["get"])],
        ),
        "gh" => {
            const READS: &[&str] = &[
                "view", "list", "ls", "status", "diff", "checks", "watch", "download",
            ];
            verb_risk(
                program,
                args,
                &["status", "search", "browse", "version", "help"],
                &[
                    ("pr", READS),
                    ("issue", READS),
                    ("run", READS),
                    ("release", READS),
                    ("repo", READS),
                    ("workflow", READS),
                    ("gist", READS),
                    ("label", READS),
                    ("cache", READS),
                    ("auth", &["status"]),
                ],
            )
        }
        _ if SYSTEM_PACKAGES.contains(&program) => verb_risk(
            program,
            args,
            &[
                "list", "info", "search", "show", "outdated", "config", "doctor", "deps", "leaves",
                "policy", "help",
            ],
            &[],
        ),
        _ => None,
    }
}

fn rm_risk(args: &[String]) -> Option<String> {
    if has_short(args, 'r') || has_short(args, 'R') || has_long(args, "--recursive") {
        return ask("`rm -r` deletes a whole tree");
    }
    let paths = operands(args);
    if paths.is_empty() {
        return ask("`rm` takes its paths from elsewhere (bulk delete)");
    }
    if paths.len() > BULK_PATHS {
        return ask(format!("`rm` of {} paths (bulk delete)", paths.len()));
    }
    paths
        .iter()
        .any(|path| path.contains(['*', '?', '[']))
        .then(|| "`rm` with a glob (bulk delete)".to_owned())
}

fn find_risk(args: &[String], place: Place<'_>, depth: usize) -> Option<String> {
    let mut words = args.iter();
    while let Some(word) = words.next() {
        match word.as_str() {
            "-delete" => return ask("`find -delete` deletes every match"),
            "-exec" | "-execdir" | "-ok" | "-okdir" => {
                let inner: Vec<String> = words
                    .by_ref()
                    .take_while(|word| !matches!(word.as_str(), ";" | "+" | "\\;"))
                    .cloned()
                    .collect();
                let Some(program) = inner.first() else {
                    continue;
                };
                if DELETERS.contains(&basename(program)) {
                    return ask(format!(
                        "`find {word} {}` deletes every match",
                        basename(program)
                    ));
                }
                if let Some(reason) = argv_risk(&inner, place, depth) {
                    return Some(format!("`find {word}`: {reason}"));
                }
            }
            _ => {}
        }
    }
    None
}

/// Global options that take a separate value.
const GIT_VALUED: &[&str] = &[
    "-C",
    "-c",
    "--git-dir",
    "--work-tree",
    "--namespace",
    "--exec-path",
    "--config-env",
];

fn git_risk(args: &[String]) -> Option<String> {
    let mut index = 0;
    while let Some(word) = args.get(index) {
        if !word.starts_with('-') {
            break;
        }
        if GIT_VALUED.contains(&word.as_str()) {
            let value = args.get(index + 1).map_or("", String::as_str);
            if word == "-c" && (value.starts_with("alias.") || value.contains('!')) {
                return ask("`git -c` defines a command");
            }
            index += 1;
        }
        index += 1;
    }
    let sub = args.get(index)?.as_str();
    let rest = &args[index + 1..];
    let reason = match sub {
        "push" => "pushes to a remote",
        "rebase" | "filter-branch" | "filter-repo" => "rewrites history",
        "prune" => "deletes unreachable objects",
        "clean" if !(has_short(rest, 'n') || has_long(rest, "--dry-run")) => {
            "deletes untracked files"
        }
        "reset" if has_long(rest, "--hard") || has_long(rest, "--merge") => {
            "discards uncommitted changes"
        }
        "checkout"
            if rest.iter().any(|word| word == "--" || word == ".")
                || has_short(rest, 'f')
                || has_long(rest, "--force") =>
        {
            "discards working-tree changes"
        }
        "restore" => {
            let staged_only = (has_short(rest, 'S') || has_long(rest, "--staged"))
                && !(has_short(rest, 'W') || has_long(rest, "--worktree"));
            if staged_only {
                return None;
            }
            "discards working-tree changes"
        }
        "switch"
            if has_short(rest, 'f')
                || has_short(rest, 'C')
                || has_long(rest, "--force")
                || has_long(rest, "--discard-changes") =>
        {
            "discards working-tree changes"
        }
        "branch"
            if ['D', 'M', 'C', 'f']
                .iter()
                .any(|flag| has_short(rest, *flag))
                || has_long(rest, "--force") =>
        {
            "force-deletes or moves a branch"
        }
        "tag"
            if has_short(rest, 'd')
                || has_short(rest, 'f')
                || has_long(rest, "--delete")
                || has_long(rest, "--force") =>
        {
            "deletes or moves a tag"
        }
        "stash" if matches!(subcommand(rest), Some("drop" | "clear")) => "drops stashed changes",
        "reflog" if matches!(subcommand(rest), Some("expire" | "delete")) => "expires the reflog",
        "gc" if rest.iter().any(|word| word.starts_with("--prune")) => {
            "deletes unreachable objects"
        }
        "update-ref" if has_short(rest, 'd') => "deletes a ref",
        "worktree"
            if subcommand(rest) == Some("remove")
                && (has_short(rest, 'f') || has_long(rest, "--force")) =>
        {
            "force-removes a worktree"
        }
        _ => return None,
    };
    ask(format!("`git {sub}` {reason}"))
}

fn node_risk(program: &str, args: &[String]) -> Option<String> {
    if has_short(args, 'g') || has_long(args, "--global") || has_long(args, "--location") {
        return ask(format!("`{program}` global install"));
    }
    match subcommand(args) {
        Some(
            "publish" | "unpublish" | "deprecate" | "dist-tag" | "owner" | "access" | "token"
            | "adduser" | "login",
        ) => ask(format!("`{program}` changes a package registry")),
        Some("install" | "i" | "in" | "isntall" | "add" | "update" | "upgrade" | "up") => ask(
            format!("`{program}` installs or changes dependencies (package scripts, network)"),
        ),
        None if program == "yarn" => ask("`yarn` installs dependencies"),
        _ => None,
    }
}

fn uv_risk(args: &[String]) -> Option<String> {
    let sub = subcommand(args)?;
    let rest: Vec<String> = args
        .iter()
        .skip_while(|word| word.as_str() != sub)
        .skip(1)
        .cloned()
        .collect();
    match (sub, subcommand(&rest)) {
        ("publish", _) => ask("`uv publish` publishes a package"),
        ("add" | "remove" | "sync", _)
        | ("pip", Some("install" | "uninstall" | "sync"))
        | ("tool", Some("install" | "uninstall" | "upgrade")) => {
            ask("`uv` installs or changes dependencies")
        }
        _ => None,
    }
}

fn container_risk(program: &str, args: &[String]) -> Option<String> {
    const READS: &[&str] = &[
        "ls", "list", "ps", "inspect", "logs", "config", "version", "images", "top", "df", "info",
        "history",
    ];
    verb_risk(
        program,
        args,
        &[
            "ps", "images", "inspect", "logs", "version", "info", "build", "history", "top",
            "stats", "search", "events", "port", "diff", "help",
        ],
        &[
            ("image", READS),
            ("container", READS),
            ("compose", READS),
            ("system", READS),
            ("volume", READS),
            ("network", READS),
        ],
    )
}

/// Asked unless the verb (or `group verb`) is one of the read verbs. An
/// option value read as the verb is asked too.
fn verb_risk(
    program: &str,
    args: &[String],
    reads: &[&str],
    groups: &[(&str, &[&str])],
) -> Option<String> {
    let Some(verb) = subcommand(args) else {
        // `--version`, `--help`, or nothing.
        return None;
    };
    if reads.contains(&verb) {
        return None;
    }
    if let Some((_, verbs)) = groups.iter().find(|(group, _)| *group == verb) {
        let rest: Vec<String> = args
            .iter()
            .skip_while(|word| word.as_str() != verb)
            .skip(1)
            .cloned()
            .collect();
        if subcommand(&rest).is_some_and(|inner| verbs.contains(&inner)) {
            return None;
        }
    }
    ask(format!(
        "`{program} {verb}` changes something outside the workspace"
    ))
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use super::{Place, Risk, assess};

    fn risk(command: &str) -> Risk {
        assess(
            command,
            Place {
                root: Path::new("/work/repo"),
                cwd: Path::new("/work/repo/sub"),
            },
        )
    }

    #[test]
    fn redirect_targets_are_resolved_against_the_working_directory() {
        assert!(risk("echo hi > out.txt").is_routine());
        assert!(risk("echo hi > ../out.txt").is_routine());
        assert!(!risk("echo hi > ../../out.txt").is_routine());
        assert!(risk("cargo test 2>&1 >/dev/null").is_routine());
        assert!(!risk("echo hi >> /etc/hosts").is_routine());
        assert!(!risk("echo hi > ~/x").is_routine());
        assert!(!risk("echo hi > \"$HOME/x\"").is_routine());
        assert!(risk("echo '>' /etc/x").is_routine(), "quoted > is text");
        assert!(!risk("cd /etc && echo hi > x").is_routine());
        assert!(risk("cd target && echo hi > x").is_routine());
        assert!(!risk("ls | tee /tmp/x").is_routine());
        assert!(risk("diff <(ls a) <(ls b) > d.txt").is_routine());
    }
}

//! Shell command analysis for the approval rules.
//!
//! Rules match parsed argv, never raw strings. A command is
//! [`CommandShape::Simple`] only when it holds nothing a shell would
//! interpret: no operators, redirections, substitutions, variables, globs or
//! wrappers. A simple command runs as exactly that argv, with no shell, so
//! what a rule approved is what executes. Everything else is
//! [`CommandShape::Compound`]: it runs under `/bin/sh -c`, no allow rule or
//! remembered choice approves it (it is always asked), and deny rules are
//! checked against every segment the analysis can find in it, including
//! the inner command of `sh -c '…'`, `env …` (`env -S '…'` included),
//! `sudo …` and `xargs …`.
//!
//! **Deny rules are advisory, not a security boundary.** They catch a
//! model naming a program it should not, in the ways this analysis can
//! see; they cannot see a program started by a script, a Makefile, a build
//! tool, an interpreter (`python -c`), a renamed copy, or a string a shell
//! assembles at run time. What actually bounds a command is the OS sandbox
//! ([`crate::sandbox`]) and the person's approval.

use std::path::{Path, PathBuf};

/// Interpreters whose `-c` argument is itself a command.
pub(crate) const SHELLS: &[&str] = &[
    "sh",
    "bash",
    "zsh",
    "dash",
    "ksh",
    "mksh",
    "fish",
    "csh",
    "tcsh",
    "pwsh",
    "powershell",
];

/// Programs that run another program given in their arguments.
const WRAPPERS: &[&str] = &[
    "env",
    "sudo",
    "doas",
    "su",
    "nohup",
    "nice",
    "time",
    "timeout",
    "xargs",
    "exec",
    "command",
    "builtin",
    "stdbuf",
    "ionice",
    "chroot",
    "caffeinate",
    "watch",
    "eval",
    "script",
    "unbuffer",
    "flock",
    "setsid",
];

/// Deepest nesting of `sh -c` the analysis unwraps.
const MAX_DEPTH: usize = 4;

/// What a command string is, for the rules.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CommandShape {
    /// Plain argv: executed directly, matchable by allow rules.
    Simple(Vec<String>),
    /// Needs a shell (or wraps another command): always asked; deny rules
    /// see `segments`.
    Compound {
        segments: Vec<Vec<String>>,
        reason: &'static str,
    },
}

impl CommandShape {
    /// Every argv a deny rule must be checked against.
    #[must_use]
    pub fn segments(&self) -> Vec<Vec<String>> {
        match self {
            Self::Simple(argv) => vec![argv.clone()],
            Self::Compound { segments, .. } => segments.clone(),
        }
    }

    #[must_use]
    pub fn is_simple(&self) -> bool {
        matches!(self, Self::Simple(_))
    }
}

pub(crate) fn basename(program: &str) -> &str {
    program.rsplit('/').next().unwrap_or(program)
}

/// Whether `text` needs a shell, and if not, its argv.
pub(crate) fn needs_shell(text: &str) -> Option<&'static str> {
    let mut single = false;
    let mut double = false;
    for character in text.chars() {
        if single {
            single = character != '\'';
            continue;
        }
        if double {
            match character {
                '"' => double = false,
                '$' | '`' | '\\' => return Some("expansion inside double quotes"),
                _ => {}
            }
            continue;
        }
        match character {
            '\'' => single = true,
            '"' => double = true,
            '|' | '&' | ';' | '\n' | '\r' => return Some("pipe, list or background operator"),
            '<' | '>' => return Some("redirection"),
            '(' | ')' | '{' | '}' => return Some("subshell or group"),
            '$' | '`' => return Some("substitution or variable"),
            '*' | '?' | '[' | ']' | '~' => return Some("glob or tilde"),
            '#' | '!' | '\\' => return Some("shell syntax"),
            _ => {}
        }
    }
    (single || double).then_some("unbalanced quotes")
}

/// Analyse one command string.
#[must_use]
pub fn analyse(text: &str) -> CommandShape {
    analyse_at(text, 0)
}

fn analyse_at(text: &str, depth: usize) -> CommandShape {
    if let Some(reason) = needs_shell(text) {
        return CommandShape::Compound {
            segments: segments_of(text, depth),
            reason,
        };
    }
    let Some(argv) = shlex::split(text).filter(|argv| !argv.is_empty()) else {
        return CommandShape::Compound {
            segments: Vec::new(),
            reason: "not a parsable command",
        };
    };
    if argv[0].contains('=') {
        return CommandShape::Compound {
            segments: segments_of(text, depth),
            reason: "environment assignment",
        };
    }
    let program = basename(&argv[0]);
    if SHELLS.contains(&program) || WRAPPERS.contains(&program) {
        return CommandShape::Compound {
            segments: unwrap_argv(argv, depth),
            reason: "runs another command",
        };
    }
    CommandShape::Simple(argv)
}

/// Split a compound command on its operators and analyse each piece.
fn segments_of(text: &str, depth: usize) -> Vec<Vec<String>> {
    let mut pieces = Vec::new();
    let mut current = String::new();
    let mut single = false;
    let mut double = false;
    for character in text.chars() {
        if single {
            single = character != '\'';
            current.push(character);
            continue;
        }
        match character {
            '\'' if !double => {
                single = true;
                current.push(character);
            }
            '"' => {
                double = !double;
                current.push(character);
            }
            // Substitutions inside double quotes still run commands.
            '`' | '(' | ')' => pieces.push(std::mem::take(&mut current)),
            '$' => {}
            '|' | '&' | ';' | '\n' | '\r' | '{' | '}' if !double => {
                pieces.push(std::mem::take(&mut current));
            }
            _ => current.push(character),
        }
    }
    pieces.push(current);
    pieces
        .iter()
        .filter_map(|piece| words_of(piece))
        .flat_map(|argv| unwrap_argv(argv, depth))
        .collect()
}

/// A piece's words without redirections and leading assignments.
fn words_of(piece: &str) -> Option<Vec<String>> {
    let words = shlex::split(piece)
        .unwrap_or_else(|| piece.split_whitespace().map(str::to_owned).collect());
    let mut argv = Vec::new();
    let mut skip_next = false;
    for word in words {
        if skip_next {
            skip_next = false;
            continue;
        }
        let redirect = word.trim_start_matches(|c: char| c.is_ascii_digit() || c == '&');
        if redirect.starts_with('>') || redirect.starts_with('<') {
            // `> file` names its target in the next word; `>file` does not.
            skip_next = redirect.trim_start_matches(['>', '<', '&', '|']).is_empty();
            continue;
        }
        if argv.is_empty() && word.contains('=') && !word.starts_with('=') {
            continue;
        }
        argv.push(word);
    }
    (!argv.is_empty()).then_some(argv)
}

/// `argv` plus whatever command it wraps.
fn unwrap_argv(argv: Vec<String>, depth: usize) -> Vec<Vec<String>> {
    let program = basename(&argv[0]).to_owned();
    let mut out = Vec::new();
    if depth < MAX_DEPTH && SHELLS.contains(&program.as_str()) {
        let script = argv
            .iter()
            .position(|word| word.starts_with('-') && !word.starts_with("--") && word.contains('c'))
            .and_then(|flag| argv.get(flag + 1));
        if let Some(script) = script {
            out.extend(analyse_at(script, depth + 1).segments());
        }
    } else if depth < MAX_DEPTH
        && program == "env"
        && let Some(split) = env_split(&argv)
    {
        // `env -S 'cmd args'` splits its argument into the command line.
        out.extend(analyse_at(&split, depth + 1).segments());
        out.extend(unwrap_wrapper(&argv, depth));
    } else if depth < MAX_DEPTH && WRAPPERS.contains(&program.as_str()) {
        out.extend(unwrap_wrapper(&argv, depth));
    }
    out.insert(0, argv);
    out
}

/// The command line `env -S` / `--split-string` builds: the split string
/// followed by the words after it.
fn env_split(argv: &[String]) -> Option<String> {
    let mut words = argv.iter().skip(1);
    while let Some(word) = words.next() {
        let value = if let Some(value) = word.strip_prefix("--split-string=") {
            value.to_owned()
        } else if word == "--split-string" {
            words.next()?.clone()
        } else if word.starts_with('-') && !word.starts_with("--") && word.contains('S') {
            let after = &word[word.find('S')? + 1..];
            if after.is_empty() {
                words.next()?.clone()
            } else {
                after.to_owned()
            }
        } else {
            continue;
        };
        let rest: Vec<String> = words
            .map(|word| {
                shlex::try_quote(word).map_or_else(|_| word.clone(), std::borrow::Cow::into_owned)
            })
            .collect();
        return Some(
            std::iter::once(value)
                .chain(rest)
                .collect::<Vec<_>>()
                .join(" "),
        );
    }
    None
}

/// Every candidate command a wrapper may run (see [`unwrap_argv`]).
fn unwrap_wrapper(argv: &[String], depth: usize) -> Vec<Vec<String>> {
    let mut out = Vec::new();
    {
        // Wrapper options may take values (`sudo -u root`, `timeout 5s`), so
        // the wrapped command could start at any later word: every suffix
        // that starts at a non-option word is a candidate. Suffixes that
        // start at another shell or wrapper are unwrapped in turn.
        for start in 1..argv.len() {
            let word = &argv[start];
            if word.starts_with('-') || word.contains('=') {
                continue;
            }
            let suffix = argv[start..].to_vec();
            let nested = basename(word);
            if SHELLS.contains(&nested) || WRAPPERS.contains(&nested) {
                out.extend(unwrap_argv(suffix, depth + 1));
            } else {
                out.push(suffix);
            }
        }
    }
    out
}

/// The directories a bare program name is looked up in: the absolute
/// entries of `PATH` only (an empty or relative entry would find a program
/// in whatever directory the command runs in, the workspace). Commands run
/// with this `PATH` too.
#[must_use]
pub fn search_path() -> Vec<PathBuf> {
    std::env::var_os("PATH")
        .map(|path| {
            std::env::split_paths(&path)
                .filter(|dir| dir.is_absolute())
                .collect()
        })
        .unwrap_or_default()
}

/// The file `program` runs from `cwd` with `search` as `PATH`, canonical;
/// `None` when it names nothing executable.
#[must_use]
pub fn resolve_program(program: &str, cwd: &Path, search: &[PathBuf]) -> Option<PathBuf> {
    use std::os::unix::fs::PermissionsExt as _;
    let executable = |path: &Path| {
        path.metadata()
            .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
    };
    let candidate = if program.contains('/') {
        let path = Path::new(program);
        let path = if path.is_absolute() {
            path.to_path_buf()
        } else {
            cwd.join(path)
        };
        path.metadata()
            .is_ok_and(|meta| meta.is_file())
            .then_some(path)?
    } else {
        search
            .iter()
            .map(|dir| dir.join(program))
            .find(|path| executable(path))?
    };
    std::fs::canonicalize(candidate).ok()
}

/// A command rule: an argv prefix. Tokens after the first compare exactly
/// (`*` matches any one token). `cargo test` matches `cargo test --all`;
/// `git push` does not match `git status`.
///
/// The program (the first token) is matched two ways:
///
/// * [`Self::matches`], for what **allows** (`command_allow`, allow and ask
///   rules, remembered choices): a name without `/` matches the same bare
///   name, and a path matches the same path; with [`Self::matches_in`],
///   also any spelling that resolves to the same file. `cargo` never
///   matches `./cargo`, a script the repository may hold.
/// * [`Self::matches_name`], for what **denies**: any path whose base name
///   is the pattern's (`rm` denies `/bin/rm` and `./rm`).
///
/// Prefix matching is what makes "always allow `cargo test`" useful; it is
/// also why a deny rule should name the program (`rm`), not one spelling of
/// its flags (`rm -rf` does not match `rm -fr`).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommandPattern(Vec<String>);

impl CommandPattern {
    /// Parse a pattern (shell words). `None` when empty or unparsable.
    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        shlex::split(text)
            .filter(|tokens| !tokens.is_empty())
            .map(Self)
    }

    #[must_use]
    pub fn from_tokens(tokens: Vec<String>) -> Option<Self> {
        (!tokens.is_empty()).then_some(Self(tokens))
    }

    #[must_use]
    pub fn tokens(&self) -> &[String] {
        &self.0
    }

    fn rest_matches(&self, argv: &[String]) -> bool {
        argv.len() >= self.0.len()
            && self
                .0
                .iter()
                .zip(argv)
                .skip(1)
                .all(|(want, have)| want == "*" || want == have)
    }

    /// Whether `argv` starts with this pattern, the program spelled the
    /// same way (see the type documentation).
    #[must_use]
    pub fn matches(&self, argv: &[String]) -> bool {
        self.rest_matches(argv) && (self.0[0] == "*" || self.0[0] == argv[0])
    }

    /// [`Self::matches`], or the program resolves (from `cwd`, through
    /// `search`) to the same file as the pattern's.
    #[must_use]
    pub fn matches_in(&self, argv: &[String], cwd: &Path, search: &[PathBuf]) -> bool {
        if !self.rest_matches(argv) {
            return false;
        }
        if self.0[0] == "*" || self.0[0] == argv[0] {
            return true;
        }
        match (
            resolve_program(&self.0[0], cwd, search),
            resolve_program(&argv[0], cwd, search),
        ) {
            (Some(want), Some(have)) => want == have,
            _ => false,
        }
    }

    /// Whether `argv` starts with this pattern, a program pattern without
    /// `/` matching any path with that base name: for deny rules.
    #[must_use]
    pub fn matches_name(&self, argv: &[String]) -> bool {
        self.rest_matches(argv)
            && (self.0[0] == "*"
                || self.0[0] == argv[0]
                || (!self.0[0].contains('/') && basename(&argv[0]) == self.0[0]))
    }

    /// This pattern with its program replaced by the file it resolves to,
    /// when it resolves: what a remembered choice stores.
    #[must_use]
    pub fn resolved(&self, cwd: &Path, search: &[PathBuf]) -> Self {
        let mut tokens = self.0.clone();
        if let Some(path) = resolve_program(&tokens[0], cwd, search) {
            tokens[0] = path.display().to_string();
        }
        Self(tokens)
    }
}

#[cfg(test)]
mod tests {
    use super::{CommandPattern, CommandShape, analyse};

    fn argv(words: &[&str]) -> Vec<String> {
        words.iter().map(|word| (*word).to_owned()).collect()
    }

    fn reason(text: &str) -> &'static str {
        match analyse(text) {
            CommandShape::Compound { reason, .. } => reason,
            CommandShape::Simple(argv) => panic!("{text} parsed as simple {argv:?}"),
        }
    }

    #[test]
    fn plain_argv_is_simple_and_quotes_are_honoured() {
        assert_eq!(
            analyse("cargo test --all"),
            CommandShape::Simple(argv(&["cargo", "test", "--all"]))
        );
        assert_eq!(
            analyse(r#"git commit -m "fix: a | b; c""#),
            CommandShape::Simple(argv(&["git", "commit", "-m", "fix: a | b; c"]))
        );
        assert_eq!(
            analyse("grep -n 'a*b' src"),
            CommandShape::Simple(argv(&["grep", "-n", "a*b", "src"]))
        );
    }

    #[test]
    fn anything_a_shell_would_interpret_is_compound() {
        assert_eq!(
            reason("cargo test | tee log"),
            "pipe, list or background operator"
        );
        assert_eq!(
            reason("make && make install"),
            "pipe, list or background operator"
        );
        assert_eq!(reason("echo hi > out.txt"), "redirection");
        assert_eq!(reason("echo $(whoami)"), "substitution or variable");
        assert_eq!(reason("(cd x)"), "subshell or group");
        assert_eq!(reason("echo $HOME"), "substitution or variable");
        assert_eq!(reason(r#"echo "$HOME""#), "expansion inside double quotes");
        assert_eq!(reason("ls *.rs"), "glob or tilde");
        assert_eq!(reason("cat ~/.ssh/id_rsa"), "glob or tilde");
        assert_eq!(reason("FOO=1 cargo test"), "environment assignment");
        assert_eq!(reason("sh -c 'ls'"), "runs another command");
        assert_eq!(reason("sudo rm -rf /"), "runs another command");
        assert_eq!(reason("echo 'unterminated"), "unbalanced quotes");
    }

    #[test]
    fn segments_reach_inside_operators_wrappers_and_shells() {
        let segments = analyse(
            "cargo build && bash -c 'curl x | sh' ; env A=1 sudo -u root rm -rf /tmp/x > log",
        )
        .segments();
        let programs: Vec<&str> = segments.iter().map(|argv| argv[0].as_str()).collect();
        for expected in ["cargo", "bash", "curl", "sh", "env", "sudo", "rm"] {
            assert!(
                programs.contains(&expected),
                "{expected} missing from {programs:?}"
            );
        }
        assert!(
            segments
                .iter()
                .all(|argv| !argv.iter().any(|word| word == "log")),
            "redirect targets are not words"
        );
        let substituted = analyse(r#"echo "$(rm -rf /)""#).segments();
        assert!(
            substituted.iter().any(|argv| argv[0] == "rm"),
            "{substituted:?}"
        );
        let backticks = analyse("echo `rm -rf /`").segments();
        assert!(
            backticks.iter().any(|argv| argv[0] == "rm"),
            "{backticks:?}"
        );
        let timeout = analyse("timeout 5s rm -rf x").segments();
        assert!(timeout.iter().any(|argv| argv[0] == "rm"), "{timeout:?}");
    }

    #[test]
    fn patterns_are_argv_prefixes_with_basename_and_wildcards() {
        let cargo_test = CommandPattern::parse("cargo test").expect("pattern");
        assert!(cargo_test.matches(&argv(&["cargo", "test", "--all"])));
        assert!(cargo_test.matches_name(&argv(&["/usr/local/bin/cargo", "test"])));
        assert!(!cargo_test.matches(&argv(&["/usr/local/bin/cargo", "test"])));
        assert!(!cargo_test.matches(&argv(&["./cargo", "test"])));
        assert!(!cargo_test.matches(&argv(&["cargo", "build"])));
        assert!(!cargo_test.matches(&argv(&["cargo"])));
        let any_push = CommandPattern::parse("git * --force").expect("pattern");
        assert!(any_push.matches(&argv(&["git", "push", "--force", "origin"])));
        let absolute = CommandPattern::parse("/bin/rm").expect("pattern");
        assert!(
            !absolute.matches(&argv(&["rm"])),
            "a path pattern matches only that path"
        );
        assert!(!absolute.matches_name(&argv(&["rm"])));
        assert!(CommandPattern::parse("  ").is_none());
    }
}

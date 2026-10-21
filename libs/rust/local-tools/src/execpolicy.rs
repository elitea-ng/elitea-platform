//! Command prefix rules: the person's `run_command` rules, as files that
//! test themselves (layer 3 of [`crate::approvals`]).
//!
//! The design follows `codex-rs/execpolicy` (openai/codex, Apache-2.0):
//! ordered **prefix rules** whose pattern elements may be lists of
//! alternatives, a decision `allow | prompt | forbidden`, a human
//! `justification`, `match` / `not_match` **examples checked when the file
//! loads**, and `host_executables` pinning a program name to the absolute
//! paths it may be given as. Only the semantics are ported, and the front
//! end is JSON, not Starlark (no code is copied).
//!
//! ```json
//! {"version": 1,
//!  "host_executables": [{"name": "git", "paths": ["/usr/bin/git"]}],
//!  "rules": [
//!    {"pattern": ["cargo", ["test", "check", "clippy", "fmt"]],
//!     "decision": "allow",
//!     "justification": "Routine Rust checks.",
//!     "match": [["cargo", "test", "-p", "x"], "cargo fmt --check"],
//!     "not_match": ["cargo publish"]},
//!    {"pattern": ["npm", "publish"], "decision": "forbidden",
//!     "justification": "Publishing goes through the release pipeline; open a PR instead.",
//!     "match": ["npm publish --tag next"]}
//!  ]}
//! ```
//!
//! * Tokens match in order, as an argv prefix; an element that is a list
//!   matches any one of its strings. `decision` defaults to `allow`.
//! * Examples are token arrays or strings split like a shell would
//!   (`shlex`). Every `match` example must match its rule and no
//!   `not_match` example may; an unknown field, an empty pattern or a bad
//!   example refuses the **whole file** ([`LoadError`]): a broken file
//!   loads no rules, so it never half-applies.
//! * A program name (no `/`) in a pattern matches the same bare name. A
//!   program given by path matches it when the name is pinned in
//!   `host_executables` and the path is listed there; with no entry for the
//!   name its base name applies (at run time an `allow` also needs the path
//!   to be the file the bare name runs: [`Resolver`]). `prompt` and
//!   `forbidden` tighten, so they match any path with that base name, pins
//!   or not.
//! * `sandbox` and `network` bound what an `allow` covers: a call asking
//!   for a wider sandbox (default bound `workspace-write`) or for the
//!   network when the rule does not name it is not covered (it falls to the
//!   defaults, which ask). They are how a remembered "always allow" keeps
//!   its "never wider" meaning.
//!
//! Not taken from the upstream design: Starlark, its CLI, policy rewriting
//! (`amend`) and the sandbox migration helpers.
//!
//! Like every deny rule ([`crate::command`]), a `forbidden` rule is
//! advisory: it sees the commands the analysis can find, not what a script
//! starts. The sandbox is the boundary.

use std::collections::HashMap;
use std::fmt;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::command::{basename, resolve_program};
use crate::policy::SandboxMode;

/// The one file format version.
pub const VERSION: u32 = 1;

/// A rules file, as stored.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct RuleFile {
    pub version: u32,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub host_executables: Vec<HostExecutable>,
    #[serde(default)]
    pub rules: Vec<PrefixRule>,
}

impl Default for RuleFile {
    fn default() -> Self {
        Self {
            version: VERSION,
            host_executables: Vec::new(),
            rules: Vec::new(),
        }
    }
}

/// A program name pinned to the absolute paths it may be given as.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct HostExecutable {
    pub name: String,
    pub paths: Vec<String>,
}

/// One pattern element: a token, or a list of alternatives.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(untagged)]
pub enum PatternToken {
    One(String),
    Any(Vec<String>),
}

impl PatternToken {
    fn alternatives(&self) -> &[String] {
        match self {
            Self::One(token) => std::slice::from_ref(token),
            Self::Any(tokens) => tokens,
        }
    }

    fn matches(&self, word: &str) -> bool {
        self.alternatives().iter().any(|token| token == word)
    }
}

impl fmt::Display for PatternToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::One(token) => formatter.write_str(token),
            Self::Any(tokens) => write!(formatter, "[{}]", tokens.join("|")),
        }
    }
}

/// What a matching rule decides.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum RuleDecision {
    #[default]
    Allow,
    Prompt,
    Forbidden,
}

impl RuleDecision {
    /// Whether the rule tightens (matches broadly: any segment, any
    /// spelling of the program).
    #[must_use]
    pub const fn tightens(self) -> bool {
        !matches!(self, Self::Allow)
    }
}

/// A `match` / `not_match` example: argv, or a command line.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(untagged)]
pub enum Example {
    Tokens(Vec<String>),
    Line(String),
}

impl Example {
    /// Its argv; `None` for a line that does not split or is empty.
    #[must_use]
    pub fn argv(&self) -> Option<Vec<String>> {
        let argv = match self {
            Self::Tokens(tokens) => tokens.clone(),
            Self::Line(line) => shlex::split(line)?,
        };
        (!argv.is_empty()).then_some(argv)
    }
}

impl fmt::Display for Example {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Line(line) => formatter.write_str(line),
            Self::Tokens(tokens) => formatter.write_str(
                &shlex::try_join(tokens.iter().map(String::as_str))
                    .unwrap_or_else(|_| tokens.join(" ")),
            ),
        }
    }
}

/// One prefix rule.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PrefixRule {
    pub pattern: Vec<PatternToken>,
    #[serde(default)]
    pub decision: RuleDecision,
    /// Shown to the person and the model; for `forbidden` it should name
    /// the alternative.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub justification: Option<String>,
    #[serde(default, rename = "match", skip_serializing_if = "Vec::is_empty")]
    pub matches: Vec<Example>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub not_match: Vec<Example>,
    /// The widest sandbox an `allow` covers (absent: `workspace-write`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sandbox: Option<SandboxMode>,
    /// Whether an `allow` covers a call that asks for the network.
    #[serde(default, skip_serializing_if = "is_false")]
    pub network: bool,
}

#[allow(clippy::trivially_copy_pass_by_ref)] // serde's skip_serializing_if signature
const fn is_false(value: &bool) -> bool {
    !*value
}

impl PrefixRule {
    /// An `allow` of exactly `tokens` (one token per element), no examples.
    #[must_use]
    pub fn allow(tokens: Vec<String>) -> Self {
        Self {
            pattern: tokens.into_iter().map(PatternToken::One).collect(),
            decision: RuleDecision::Allow,
            justification: None,
            matches: Vec::new(),
            not_match: Vec::new(),
            sandbox: None,
            network: false,
        }
    }

    /// The pattern, as the panel shows it (`cargo [test|check]`).
    #[must_use]
    pub fn display_pattern(&self) -> String {
        self.pattern
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>()
            .join(" ")
    }

    /// Whether an `allow` covers a call with `sandbox` and `network`.
    #[must_use]
    pub fn covers(&self, sandbox: SandboxMode, network: bool) -> bool {
        sandbox <= self.sandbox.unwrap_or(SandboxMode::WorkspaceWrite) && (!network || self.network)
    }
}

/// Where a rule file belongs (layer 3 reads the workspace's, then the
/// person's).
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RuleScope {
    /// The workspace's own rules (in the host's data for that folder).
    Workspace,
    /// The person's rules for every workspace.
    User,
}

impl RuleScope {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Workspace => "workspace",
            Self::User => "user",
        }
    }
}

/// Why a rule file was refused: the whole file loads nothing.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct LoadError {
    /// The file, as the host names it.
    pub file: String,
    /// The rule (1-based), when one rule is at fault.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rule_index: Option<usize>,
    /// The example at fault.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub example: Option<String>,
    /// `match` or `not_match`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expected: Option<String>,
    /// What the rule did with the example.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub got: Option<String>,
    /// For a person.
    pub message: String,
}

impl fmt::Display for LoadError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}: {}", self.file, self.message)
    }
}

impl std::error::Error for LoadError {}

/// How an `allow` checks a program given by path at run time: from the
/// command's working directory, through the absolute `PATH` entries.
#[derive(Clone, Copy, Debug)]
pub struct Resolver<'a> {
    pub cwd: &'a Path,
    pub search: &'a [PathBuf],
}

/// A validated rule file, ready to match.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct RuleSet {
    file: RuleFile,
    pins: HashMap<String, Vec<String>>,
}

/// One rule of a set that matched.
#[derive(Clone, Copy, Debug)]
pub struct Hit<'a> {
    /// 1-based.
    pub index: usize,
    pub rule: &'a PrefixRule,
}

impl RuleSet {
    /// Parse and validate `bytes` (`file` names it in errors).
    ///
    /// # Errors
    ///
    /// The file does not parse, has an unknown field, or a rule's examples
    /// disagree with it.
    pub fn parse(file: &str, bytes: &[u8]) -> Result<Self, Box<LoadError>> {
        let parsed: RuleFile = serde_json::from_slice(bytes).map_err(|error| {
            Box::new(LoadError {
                file: file.to_owned(),
                message: format!("not a valid rules file: {error}"),
                ..LoadError::default()
            })
        })?;
        Self::compile(file, parsed)
    }

    /// Validate a parsed file.
    ///
    /// # Errors
    ///
    /// As [`Self::parse`].
    pub fn compile(file: &str, parsed: RuleFile) -> Result<Self, Box<LoadError>> {
        let refuse = |rule_index: Option<usize>, message: String| {
            Box::new(LoadError {
                file: file.to_owned(),
                rule_index,
                message,
                ..LoadError::default()
            })
        };
        if parsed.version != VERSION {
            return Err(refuse(
                None,
                format!(
                    "version {} is not supported (expected {VERSION})",
                    parsed.version
                ),
            ));
        }
        let mut pins: HashMap<String, Vec<String>> = HashMap::new();
        for pin in &parsed.host_executables {
            if pin.name.is_empty() || pin.name.contains('/') {
                return Err(refuse(
                    None,
                    format!(
                        "host_executables name `{}` must be a bare program name",
                        pin.name
                    ),
                ));
            }
            if let Some(path) = pin
                .paths
                .iter()
                .find(|path| !Path::new(path).is_absolute() || basename(path) != pin.name)
            {
                return Err(refuse(
                    None,
                    format!(
                        "host_executables path `{path}` must be an absolute path to `{}`",
                        pin.name
                    ),
                ));
            }
            pins.entry(pin.name.clone())
                .or_default()
                .extend(pin.paths.iter().cloned());
        }
        let set = Self { file: parsed, pins };
        for (position, rule) in set.file.rules.iter().enumerate() {
            let index = position + 1;
            if rule.pattern.is_empty() {
                return Err(refuse(Some(index), "the pattern is empty".to_owned()));
            }
            if rule.pattern.iter().any(|token| {
                token.alternatives().is_empty() || token.alternatives().iter().any(String::is_empty)
            }) {
                return Err(refuse(Some(index), "a pattern element is empty".to_owned()));
            }
            for (examples, expected) in [(&rule.matches, true), (&rule.not_match, false)] {
                for example in examples {
                    let label = if expected { "match" } else { "not_match" };
                    let Some(argv) = example.argv() else {
                        return Err(Box::new(LoadError {
                            file: file.to_owned(),
                            rule_index: Some(index),
                            example: Some(example.to_string()),
                            expected: Some(label.to_owned()),
                            got: Some("not a command".to_owned()),
                            message: format!(
                                "rule {index}: the {label} example `{example}` is not a command"
                            ),
                        }));
                    };
                    let got = set.matches(rule, &argv, None);
                    if got != expected {
                        return Err(Box::new(LoadError {
                            file: file.to_owned(),
                            rule_index: Some(index),
                            example: Some(example.to_string()),
                            expected: Some(label.to_owned()),
                            got: Some(if got { "matched" } else { "did not match" }.to_owned()),
                            message: format!(
                                "rule {index} (`{}`): the {label} example `{example}` {}",
                                rule.display_pattern(),
                                if got {
                                    "matches it"
                                } else {
                                    "does not match it"
                                }
                            ),
                        }));
                    }
                }
            }
        }
        Ok(set)
    }

    /// The file as validated (what the panel lists and what is written back).
    #[must_use]
    pub fn file(&self) -> &RuleFile {
        &self.file
    }

    #[must_use]
    pub fn rules(&self) -> &[PrefixRule] {
        &self.file.rules
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.file.rules.is_empty()
    }

    /// Whether `rule` (one of this set's) matches `argv`. `resolver`: the
    /// run-time check of a path-given program an `allow` needs; `None`
    /// (when examples are validated) matches by the file alone.
    #[must_use]
    pub fn matches(
        &self,
        rule: &PrefixRule,
        argv: &[String],
        resolver: Option<Resolver<'_>>,
    ) -> bool {
        let Some((program, rest)) = rule.pattern.split_first() else {
            return false;
        };
        let Some((have, words)) = argv.split_first() else {
            return false;
        };
        words.len() >= rest.len()
            && rest
                .iter()
                .zip(words)
                .all(|(want, word)| want.matches(word))
            && program
                .alternatives()
                .iter()
                .any(|want| self.program_matches(want, have, rule.decision.tightens(), resolver))
    }

    fn program_matches(
        &self,
        want: &str,
        have: &str,
        broad: bool,
        resolver: Option<Resolver<'_>>,
    ) -> bool {
        if want == have {
            return true;
        }
        let same_file = |resolver: Resolver<'_>| match (
            resolve_program(want, resolver.cwd, resolver.search),
            resolve_program(have, resolver.cwd, resolver.search),
        ) {
            (Some(want), Some(have)) => want == have,
            _ => false,
        };
        if want.contains('/') {
            // A path pattern names that file only.
            return resolver.is_some_and(same_file);
        }
        if !have.contains('/') || basename(have) != want {
            return false;
        }
        if broad {
            return true;
        }
        if let Some(paths) = self.pins.get(want) {
            if paths.iter().any(|path| path == have) {
                return true;
            }
            // The listed file, spelled another way.
            return resolver.is_some_and(|resolver| {
                resolve_program(have, resolver.cwd, resolver.search).is_some_and(|file| {
                    paths
                        .iter()
                        .any(|path| std::fs::canonicalize(path).is_ok_and(|listed| listed == file))
                })
            });
        }
        match resolver {
            // Unpinned at run time: the file the bare name runs, not a
            // look-alike (`./cargo` the repository holds).
            Some(resolver) => same_file(resolver),
            None => Path::new(have).is_absolute(),
        }
    }

    /// The rules matching one argv, in file order.
    pub fn hits<'a>(
        &'a self,
        argv: &'a [String],
        resolver: Option<Resolver<'a>>,
    ) -> impl Iterator<Item = Hit<'a>> + 'a {
        self.file
            .rules
            .iter()
            .enumerate()
            .filter(move |(_, rule)| self.matches(rule, argv, resolver))
            .map(|(position, rule)| Hit {
                index: position + 1,
                rule,
            })
    }

    /// This file with `rule` appended, validated.
    ///
    /// # Errors
    ///
    /// The new rule's examples disagree with it.
    pub fn with_rule(&self, file: &str, rule: PrefixRule) -> Result<Self, Box<LoadError>> {
        let mut next = self.file.clone();
        if !next.rules.contains(&rule) {
            next.rules.push(rule);
        }
        Self::compile(file, next)
    }
}

/// Today's date (UTC), `YYYY-MM-DD`: when an "always allow" was written.
#[must_use]
pub fn today() -> String {
    let seconds = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_secs());
    civil_date(i64::try_from(seconds / 86_400).unwrap_or_default())
}

/// The proleptic Gregorian date `days` after 1970-01-01.
fn civil_date(days: i64) -> String {
    let shifted = days + 719_468;
    let era = shifted.div_euclid(146_097);
    let day_of_era = shifted.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_index = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * month_index + 2) / 5 + 1;
    let month = if month_index < 10 {
        month_index + 3
    } else {
        month_index - 9
    };
    let year = year_of_era + era * 400 + i64::from(month <= 2);
    format!("{year:04}-{month:02}-{day:02}")
}

#[cfg(test)]
mod tests {
    use super::{Example, PatternToken, PrefixRule, RuleDecision, RuleSet, civil_date};

    #[test]
    fn civil_dates() {
        assert_eq!(civil_date(0), "1970-01-01");
        assert_eq!(civil_date(20_454), "2026-01-01");
        assert_eq!(civil_date(11_016), "2000-02-29");
    }

    fn argv(line: &str) -> Vec<String> {
        shlex::split(line).expect("argv")
    }

    const ISSUE_EXAMPLE: &str = r#"{"version": 1,
      "host_executables": [{"name": "git", "paths": ["/usr/bin/git", "/opt/homebrew/bin/git"]}],
      "rules": [
        {"pattern": ["cargo", ["test", "check", "clippy", "fmt"]],
         "decision": "allow",
         "justification": "Routine Rust checks.",
         "match": [["cargo", "test", "-p", "x"], "cargo fmt --check"],
         "not_match": ["cargo publish"]},
        {"pattern": ["npm", "publish"], "decision": "forbidden",
         "justification": "Publishing goes through the release pipeline; open a PR instead.",
         "match": ["npm publish --tag next"]}
      ]}"#;

    #[test]
    fn the_documented_file_loads() {
        let set = RuleSet::parse("rules.json", ISSUE_EXAMPLE.as_bytes()).expect("loads");
        assert_eq!(set.rules().len(), 2);
        assert_eq!(
            set.rules()[0].display_pattern(),
            "cargo [test|check|clippy|fmt]"
        );
        assert_eq!(set.rules()[1].decision, RuleDecision::Forbidden);
        let minimal = RuleSet::parse("r", br#"{"version":1,"rules":[{"pattern":["ls"]}]}"#)
            .expect("decision defaults to allow");
        assert_eq!(minimal.rules()[0].decision, RuleDecision::Allow);
    }

    #[test]
    fn examples_validated_at_load() {
        let refused =
            |text: &str| RuleSet::parse("workspace rules", text.as_bytes()).expect_err("refused");

        let bad_match = refused(
            r#"{"version":1,"rules":[
              {"pattern":["ls"]},
              {"pattern":["cargo","test"],"match":["cargo build"]}]}"#,
        );
        assert_eq!(bad_match.file, "workspace rules");
        assert_eq!(bad_match.rule_index, Some(2));
        assert_eq!(bad_match.example.as_deref(), Some("cargo build"));
        assert_eq!(bad_match.expected.as_deref(), Some("match"));
        assert_eq!(bad_match.got.as_deref(), Some("did not match"));

        let bad_not_match = refused(
            r#"{"version":1,"rules":[{"pattern":["cargo",["test","check"]],
              "not_match":[["cargo","check","--all"]]}]}"#,
        );
        assert_eq!(bad_not_match.rule_index, Some(1));
        assert_eq!(bad_not_match.example.as_deref(), Some("cargo check --all"));
        assert_eq!(bad_not_match.expected.as_deref(), Some("not_match"));
        assert_eq!(bad_not_match.got.as_deref(), Some("matched"));

        let unknown = refused(r#"{"version":1,"rules":[{"pattern":["ls"],"verdict":"allow"}]}"#);
        assert!(unknown.message.contains("verdict"), "{}", unknown.message);
        assert!(unknown.rule_index.is_none());
        let unknown_top = refused(r#"{"version":1,"rules":[],"extra":true}"#);
        assert!(
            unknown_top.message.contains("extra"),
            "{}",
            unknown_top.message
        );

        let empty = refused(r#"{"version":1,"rules":[{"pattern":["ls"]},{"pattern":[]}]}"#);
        assert_eq!(empty.rule_index, Some(2));
        let empty_alternatives = refused(r#"{"version":1,"rules":[{"pattern":["git",[]]}]}"#);
        assert_eq!(empty_alternatives.rule_index, Some(1));
        let unparsable = refused(r#"{"version":1,"rules":[{"pattern":["ls"],"match":["ls 'x"]}]}"#);
        assert_eq!(unparsable.got.as_deref(), Some("not a command"));
        let version = refused(r#"{"version":2,"rules":[]}"#);
        assert!(version.message.contains("version 2"));
        let bad_pin =
            refused(r#"{"version":1,"host_executables":[{"name":"git","paths":["git"]}]}"#);
        assert!(bad_pin.message.contains("absolute"));
        let bad_decision =
            refused(r#"{"version":1,"rules":[{"pattern":["ls"],"decision":"deny"}]}"#);
        assert!(bad_decision.message.contains("deny"));
    }

    #[test]
    fn alternatives_and_prefix_order() {
        let set = RuleSet::parse(
            "r",
            br#"{"version":1,"rules":[
              {"pattern":[["git","hg"],["status","log"],"--short"]},
              {"pattern":["cargo"],"decision":"prompt"}]}"#,
        )
        .expect("loads");
        let rule = &set.rules()[0];
        for hit in ["git status --short", "hg log --short x", "git log --short"] {
            assert!(set.matches(rule, &argv(hit), None), "{hit}");
        }
        for miss in [
            "git --short status",
            "git status",
            "svn status --short",
            "git diff --short",
            "status git --short",
        ] {
            assert!(!set.matches(rule, &argv(miss), None), "{miss}");
        }
        // Hits come in file order, 1-based.
        let hits: Vec<usize> = set
            .hits(&argv("cargo test"), None)
            .map(|hit| hit.index)
            .collect();
        assert_eq!(hits, vec![2]);
        // The pattern is a prefix: more words after it still match.
        assert!(set.matches(&set.rules()[1], &argv("cargo build --release"), None));
        assert!(!set.matches(&set.rules()[1], &[], None));
    }

    /// Ported from the semantics in the upstream README: a basename rule
    /// applies to an absolute path only when `host_executables` lists it;
    /// with no entry for the name the basename applies.
    #[test]
    fn host_executable_pins_absolute_paths() {
        let set = RuleSet::parse(
            "r",
            br#"{"version":1,
              "host_executables":[{"name":"git","paths":["/usr/bin/git"]}],
              "rules":[
                {"pattern":["git","status"],
                 "match":["git status","/usr/bin/git status"],
                 "not_match":["/tmp/evil/git status","./git status"]},
                {"pattern":["cargo","test"],"match":["/opt/x/cargo test"]},
                {"pattern":["git","push"],"decision":"forbidden","match":["/tmp/evil/git push"]}]}"#,
        )
        .expect("the examples agree");
        let git = &set.rules()[0];
        assert!(!set.matches(git, &argv("/tmp/evil/git status"), None));
        assert!(set.matches(git, &argv("/usr/bin/git status"), None));
        assert!(set.matches(git, &argv("git status"), None));
        assert!(
            set.matches(&set.rules()[1], &argv("/opt/x/cargo test"), None),
            "unpinned: the basename applies"
        );
        assert!(
            set.matches(&set.rules()[2], &argv("/tmp/evil/git push"), None),
            "a forbidden rule tightens: pins never narrow it"
        );

        // A pinned allow whose example names an unlisted path is refused.
        let refused = RuleSet::parse(
            "r",
            br#"{"version":1,"host_executables":[{"name":"git","paths":["/usr/bin/git"]}],
              "rules":[{"pattern":["git"],"match":["/tmp/evil/git status"]}]}"#,
        )
        .expect_err("refused");
        assert_eq!(refused.got.as_deref(), Some("did not match"));
    }

    #[test]
    fn appended_rules_are_validated_and_not_duplicated() {
        let set = RuleSet::default();
        let mut rule = PrefixRule::allow(vec!["cargo".into(), "test".into()]);
        rule.matches = vec![Example::Tokens(vec![
            "cargo".into(),
            "test".into(),
            "--all".into(),
        ])];
        let set = set.with_rule("r", rule.clone()).expect("valid");
        let set = set.with_rule("r", rule.clone()).expect("valid");
        assert_eq!(set.rules().len(), 1);
        rule.matches = vec![Example::Line("cargo build".into())];
        assert!(set.with_rule("r", rule).is_err());
        assert_eq!(
            PatternToken::Any(vec!["a".into(), "b".into()]).to_string(),
            "[a|b]"
        );
    }
}

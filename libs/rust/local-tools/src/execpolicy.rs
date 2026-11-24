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
//!   matches any one of its strings, and the element `{"any": true}`
//!   matches any one word (the `*` of an older host's command rules; a
//!   string `"*"` is the literal word `*`). `decision` defaults to `allow`.
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
//! * `origin: "repository"` marks a rule imported from a repository's
//!   proposal: its justification is the repository's text, so the person
//!   sees it but the model never does.
//! * On an `allow` only (refused on `prompt` and `forbidden`, which already
//!   match broadly):
//!   * `conditions` ([`crate::conditions`], #3007): what the command's
//!     flags must look like anywhere after the program (`forbid_flags`,
//!     `require_flags`, `flag_values`, `graphql: "query"`). The examples
//!     are checked through them.
//!   * `hosts` (#3005): the hosts the command may reach; it runs sandboxed
//!     with the network off but for a filtering proxy that tunnels to
//!     those only ([`crate::egress_proxy`]). Such a rule covers a call that
//!     asks for the network where the platform enforces the list (macOS).
//!     Refused together with `network: true`.
//!   * `credential` (#3006): a built-in profile ([`crate::credentials`],
//!     `"gh"`) whose program (the pattern's first word, exactly) gets its
//!     stored login inside the sandbox; its network is limited to `hosts`,
//!     or the profile's hosts. Only a rule the person wrote grants it
//!     ([`crate::approvals::RulesEngine::command_grant`]).
//!
//! Not taken from the upstream design: Starlark, its CLI, policy rewriting
//! (`amend`) and the sandbox migration helpers.
//!
//! Like every deny rule ([`crate::command`]), a `forbidden` rule is
//! advisory: it sees the commands the analysis can find, not what a script
//! starts. The sandbox is the boundary.

use std::cell::RefCell;
use std::collections::HashMap;
use std::fmt;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::command::{basename, resolve_program};
use crate::conditions::Conditions;
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

/// One pattern element: a token, a list of alternatives, or any one word.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(untagged)]
pub enum PatternToken {
    One(String),
    OneOf(Vec<String>),
    /// `{"any": true}`: any one word.
    Wildcard(AnyWord),
}

/// The wildcard element, `{"any": true}` (`any: false` is refused).
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct AnyWord {
    pub any: bool,
}

impl PatternToken {
    /// The wildcard element.
    pub const ANY: Self = Self::Wildcard(AnyWord { any: true });

    /// A word of an older host's command rule ([`crate::command::CommandPattern`]),
    /// where `*` is any one word.
    #[must_use]
    pub fn from_command_word(word: String) -> Self {
        if word == "*" {
            Self::ANY
        } else {
            Self::One(word)
        }
    }

    fn alternatives(&self) -> &[String] {
        match self {
            Self::One(token) => std::slice::from_ref(token),
            Self::OneOf(tokens) => tokens,
            Self::Wildcard(_) => &[],
        }
    }

    /// Not empty, no empty alternative, not `{"any": false}`.
    fn is_valid(&self) -> bool {
        match self {
            Self::Wildcard(AnyWord { any }) => *any,
            _ => {
                !self.alternatives().is_empty() && !self.alternatives().iter().any(String::is_empty)
            }
        }
    }

    const fn is_wildcard(&self) -> bool {
        matches!(self, Self::Wildcard(_))
    }

    fn matches(&self, word: &str) -> bool {
        self.is_wildcard() || self.alternatives().iter().any(|token| token == word)
    }
}

/// A word as the panel shows it: quoted like a shell would when it is not
/// plain (so `*`, the wildcard, and `'*'`, the literal word, differ).
fn shown(word: &str) -> String {
    let plain = !word.is_empty()
        && word
            .chars()
            .all(|c| c.is_alphanumeric() || "_@%+=:,./-".contains(c));
    if plain {
        word.to_owned()
    } else {
        format!("'{}'", word.replace('\'', "'\"'\"'"))
    }
}

impl fmt::Display for PatternToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::One(token) => formatter.write_str(&shown(token)),
            Self::OneOf(tokens) => write!(
                formatter,
                "[{}]",
                tokens
                    .iter()
                    .map(|token| shown(token))
                    .collect::<Vec<_>>()
                    .join("|")
            ),
            Self::Wildcard(_) => formatter.write_str("*"),
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
    /// Where the rule came from, when not the person (`repository`: an
    /// imported proposal; its justification never reaches the model).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin: Option<RuleOrigin>,
    /// `allow` only: the built-in credential profile
    /// ([`crate::credentials`]) the command gets its program's login from,
    /// inside the sandbox (`"gh"`). Its network is limited to
    /// [`Self::hosts`], or the profile's hosts when the rule names none.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credential: Option<String>,
    /// `allow` only: the hosts the command may reach (exact names or
    /// `*.parent`), through the host's filtering proxy; the network stays
    /// off for everything else ([`crate::egress_proxy`]). Such a rule
    /// covers a call that asks for the network, where the platform can
    /// enforce the list.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub hosts: Vec<String>,
    /// `allow` only: what the command's flags must look like
    /// ([`crate::conditions`]).
    #[serde(default, skip_serializing_if = "Conditions::is_empty")]
    pub conditions: Conditions,
}

/// Where a rule came from, when it was not written by the person.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RuleOrigin {
    /// Imported from a repository's `.elitea/rules.json`.
    Repository,
}

/// The most `match` (or `not_match`) examples a remembered rule collects.
pub const MAX_EXAMPLES: usize = 8;

#[allow(clippy::trivially_copy_pass_by_ref)] // serde's skip_serializing_if signature
const fn is_false(value: &bool) -> bool {
    !*value
}

impl PrefixRule {
    /// An `allow` of the words of an older host's command rule, one
    /// element per word (`*`: any one word), no examples.
    #[must_use]
    pub fn from_command_pattern(tokens: Vec<String>) -> Self {
        Self {
            pattern: tokens
                .into_iter()
                .map(PatternToken::from_command_word)
                .collect(),
            decision: RuleDecision::Allow,
            justification: None,
            matches: Vec::new(),
            not_match: Vec::new(),
            sandbox: None,
            network: false,
            origin: None,
            credential: None,
            hosts: Vec::new(),
            conditions: Conditions::default(),
        }
    }

    /// Whether `other` is the same rule but for its justification and
    /// examples (what "always allow" merges into instead of adding).
    #[must_use]
    pub fn same_scope(&self, other: &Self) -> bool {
        self.pattern == other.pattern
            && self.decision == other.decision
            && self.sandbox.unwrap_or(SandboxMode::WorkspaceWrite)
                == other.sandbox.unwrap_or(SandboxMode::WorkspaceWrite)
            && self.network == other.network
            && self.credential == other.credential
            && self.hosts == other.hosts
            && self.conditions == other.conditions
    }

    /// Whether the rule limits its command's network to named hosts (its
    /// own, or its credential profile's).
    #[must_use]
    pub fn scopes_network(&self) -> bool {
        !self.hosts.is_empty() || self.credential.is_some()
    }

    /// The hosts the command may reach: the rule's, else its credential
    /// profile's; empty when the rule does not scope the network.
    #[must_use]
    pub fn effective_hosts(&self) -> Vec<String> {
        if !self.hosts.is_empty() {
            return self.hosts.clone();
        }
        self.credential
            .as_deref()
            .and_then(crate::credentials::profile)
            .map(|profile| {
                profile
                    .default_hosts
                    .iter()
                    .map(|host| (*host).to_owned())
                    .collect()
            })
            .unwrap_or_default()
    }

    /// Why the rule's grant fields (`credential`, `hosts`, `conditions`)
    /// are malformed, or `None`.
    fn grant_problem(&self) -> Option<String> {
        let grants = self.credential.is_some() || !self.hosts.is_empty();
        if self.decision != RuleDecision::Allow && (grants || !self.conditions.is_empty()) {
            return Some(
                "credential, hosts and conditions belong to an allow rule (prompt and forbidden \
                 rules already match broadly)"
                    .to_owned(),
            );
        }
        if let Some(problem) = self.conditions.problem() {
            return Some(problem);
        }
        if self.network && grants {
            return Some(
                "`hosts` and `credential` limit the network to named hosts; drop `network: true`"
                    .to_owned(),
            );
        }
        if self.hosts.len() > crate::egress_proxy::MAX_HOSTS {
            return Some(format!("at most {} hosts", crate::egress_proxy::MAX_HOSTS));
        }
        if let Some(problem) = self
            .hosts
            .iter()
            .find_map(|host| crate::egress_proxy::host_problem(host))
        {
            return Some(problem);
        }
        if let Some(id) = &self.credential {
            let Some(profile) = crate::credentials::profile(id) else {
                return Some(format!(
                    "`{id}` is not a credential profile (known: {})",
                    crate::credentials::PROFILES
                        .iter()
                        .map(|profile| profile.id)
                        .collect::<Vec<_>>()
                        .join(", ")
                ));
            };
            if self.pattern.first() != Some(&PatternToken::One(profile.program.to_owned())) {
                return Some(format!(
                    "the `{id}` credential is for `{}`: the pattern must start with exactly that word",
                    profile.program
                ));
            }
        }
        None
    }

    /// Whether the person wrote it (or vouched for it): its justification
    /// may reach the model.
    #[must_use]
    pub const fn trusted(&self) -> bool {
        self.origin.is_none()
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

    /// Whether an `allow` covers a call with `sandbox` and `network`. A
    /// rule that limits the network to named hosts ([`Self::scopes_network`])
    /// covers a network call only where its grant applies
    /// ([`crate::approvals::RulesEngine::command_grant`]); this says
    /// whether it covers one without that.
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
    /// Command rules of an older host's workspace settings, migrated (not
    /// a file: they are numbered on their own, `legacy#1`).
    Legacy,
}

impl RuleScope {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Workspace => "workspace",
            Self::User => "user",
            Self::Legacy => "legacy",
        }
    }
}

impl RuleFile {
    /// Add `rule`, or merge it into the rule of the same scope
    /// ([`PrefixRule::same_scope`]): its new examples are appended there
    /// (at most [`MAX_EXAMPLES`] each), its justification and origin are
    /// the existing rule's. Returns whether the file changed.
    pub fn add_rule(&mut self, rule: PrefixRule) -> bool {
        let Some(existing) = self
            .rules
            .iter_mut()
            .find(|current| current.same_scope(&rule))
        else {
            self.rules.push(rule);
            return true;
        };
        let mut changed = false;
        for (have, add) in [
            (&mut existing.matches, rule.matches),
            (&mut existing.not_match, rule.not_match),
        ] {
            for example in add {
                if have.len() < MAX_EXAMPLES && !have.contains(&example) {
                    have.push(example);
                    changed = true;
                }
            }
        }
        changed
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
/// command's working directory, through the absolute `PATH` entries. Each
/// program is looked up once per resolver's cache (one decision), however
/// many rules and segments ask.
#[derive(Clone, Copy, Debug)]
pub struct Resolver<'a> {
    pub cwd: &'a Path,
    pub search: &'a [PathBuf],
    pub cache: &'a ResolveCache,
}

/// The files programs resolved to, for one decision.
#[derive(Debug, Default)]
pub struct ResolveCache(RefCell<HashMap<String, Option<PathBuf>>>);

impl ResolveCache {
    /// How many programs were looked up (tests).
    #[must_use]
    pub fn len(&self) -> usize {
        self.0.borrow().len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.0.borrow().is_empty()
    }
}

impl Resolver<'_> {
    /// The file `program` runs, looked up once.
    fn resolve(&self, program: &str) -> Option<PathBuf> {
        if let Some(found) = self.cache.0.borrow().get(program) {
            return found.clone();
        }
        let found = resolve_program(program, self.cwd, self.search);
        self.cache
            .0
            .borrow_mut()
            .insert(program.to_owned(), found.clone());
        found
    }
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
            if !rule.pattern.iter().all(PatternToken::is_valid) {
                return Err(refuse(
                    Some(index),
                    "a pattern element is empty (or `{\"any\": false}`)".to_owned(),
                ));
            }
            if let Some(problem) = rule.grant_problem() {
                return Err(refuse(Some(index), format!("rule {index}: {problem}")));
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
            && (program.is_wildcard()
                || program.alternatives().iter().any(|want| {
                    self.program_matches(want, have, rule.decision.tightens(), resolver)
                }))
            && rule.conditions.hold(words)
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
        let same_file =
            |resolver: Resolver<'_>| match (resolver.resolve(want), resolver.resolve(have)) {
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
                resolver.resolve(have).is_some_and(|file| {
                    paths
                        .iter()
                        .any(|path| resolver.resolve(path).is_some_and(|listed| listed == file))
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

    /// This file with `rule` added ([`RuleFile::add_rule`]), validated.
    ///
    /// # Errors
    ///
    /// The new rule's examples disagree with it.
    pub fn with_rule(&self, file: &str, rule: PrefixRule) -> Result<Self, Box<LoadError>> {
        let mut next = self.file.clone();
        next.add_rule(rule);
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

#[cfg(all(test, unix))]
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
        let mut rule = PrefixRule::from_command_pattern(vec!["cargo".into(), "test".into()]);
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
            PatternToken::OneOf(vec!["a".into(), "b".into()]).to_string(),
            "[a|b]"
        );
    }

    /// The same prefix remembered again (another justification, another
    /// example) merges into the rule it already has, examples bounded.
    #[test]
    fn the_same_prefix_merges_instead_of_piling_up() {
        let remembered = |example: &str, justification: &str| {
            let mut rule = PrefixRule::from_command_pattern(vec!["git".into(), "reset".into()]);
            rule.justification = Some(justification.to_owned());
            rule.matches = vec![Example::Line(example.to_owned())];
            rule
        };
        let mut set = RuleSet::default();
        for day in 0..20 {
            set = set
                .with_rule(
                    "r",
                    remembered(&format!("git reset --hard v{day}"), &format!("day {day}")),
                )
                .expect("valid");
        }
        assert_eq!(set.rules().len(), 1, "one rule for one prefix");
        let rule = &set.rules()[0];
        assert_eq!(rule.justification.as_deref(), Some("day 0"));
        assert_eq!(rule.matches.len(), super::MAX_EXAMPLES);
        assert_eq!(rule.matches[1], Example::Line("git reset --hard v1".into()));
        // Another bound is another rule.
        let mut network = remembered("git reset x", "net");
        network.network = true;
        let set = set.with_rule("r", network).expect("valid");
        assert_eq!(set.rules().len(), 2);
        let mut forbidden = remembered("git reset x", "no");
        forbidden.decision = RuleDecision::Forbidden;
        assert_eq!(
            set.with_rule("r", forbidden).expect("valid").rules().len(),
            3
        );
    }

    /// `{"any": true}` is any one word; `"*"` is the word `*`; an older
    /// host's `*` becomes the wildcard.
    #[test]
    fn wildcard_elements() {
        let set = RuleSet::parse(
            "r",
            br#"{"version":1,"rules":[
              {"pattern":["git",{"any":true},"--force"],"decision":"forbidden",
               "match":["git push --force","git reset --force x"],"not_match":["git push origin"]},
              {"pattern":["echo","*"],"match":["echo '*'"],"not_match":["echo hi"]},
              {"pattern":[{"any":true},"--version"],"match":["/usr/bin/anything --version"]}]}"#,
        )
        .expect("loads");
        assert_eq!(set.rules()[0].display_pattern(), "git * --force");
        assert_eq!(set.rules()[1].display_pattern(), "echo '*'");
        let migrated =
            PrefixRule::from_command_pattern(vec!["git".into(), "*".into(), "--force".into()]);
        assert_eq!(migrated.pattern, set.rules()[0].pattern);
        let text = serde_json::to_string(&migrated.pattern).expect("json");
        assert_eq!(text, r#"["git",{"any":true},"--force"]"#);
        assert!(
            RuleSet::parse(
                "r",
                br#"{"version":1,"rules":[{"pattern":["git",{"any":false}]}]}"#
            )
            .is_err()
        );
        assert!(
            RuleSet::parse(
                "r",
                br#"{"version":1,"rules":[{"pattern":["git",{"any":true,"x":1}]}]}"#
            )
            .is_err()
        );
        assert_eq!(
            PatternToken::One("a b|c".into()).to_string(),
            "'a b|c'",
            "a word that is not plain is shown quoted"
        );
    }

    /// One decision looks each program up once, however many rules ask.
    #[test]
    fn a_resolver_looks_each_program_up_once() {
        let set = RuleSet::parse(
            "r",
            br#"{"version":1,"rules":[{"pattern":["sh","a"]},{"pattern":["sh","b"]},{"pattern":["sh","c"]}]}"#,
        )
        .expect("loads");
        let cache = super::ResolveCache::default();
        let search = vec![
            std::path::PathBuf::from("/bin"),
            std::path::PathBuf::from("/usr/bin"),
        ];
        let resolver = super::Resolver {
            cwd: std::path::Path::new("/"),
            search: &search,
            cache: &cache,
        };
        let argv = argv("/bin/sh a");
        for _ in 0..3 {
            let _ = set.hits(&argv, Some(resolver)).count();
        }
        assert!(
            cache.len() <= 2,
            "sh and /bin/sh, once each: {}",
            cache.len()
        );
    }

    fn loads(rule: &str) -> Result<RuleSet, Box<super::LoadError>> {
        RuleSet::parse(
            "user",
            format!(r#"{{"version":1,"rules":[{rule}]}}"#).as_bytes(),
        )
    }

    #[test]
    fn grant_fields_load_on_allow_rules_only() {
        let set = loads(
            r#"{"pattern":["gh",["pr","issue"]],"credential":"gh",
                "hosts":["api.github.com","*.githubusercontent.com"],
                "conditions":{"forbid_flags":["-f"],"flag_values":{"-X":["GET"]}},
                "match":["gh pr list","gh pr view 1 -X GET"],
                "not_match":["gh pr view 1 -f a=b","gh pr view -XPOST 1"]}"#,
        )
        .expect("loads");
        let rule = &set.rules()[0];
        assert!(rule.scopes_network());
        assert_eq!(
            rule.effective_hosts(),
            vec!["api.github.com", "*.githubusercontent.com"]
        );
        let defaults = loads(r#"{"pattern":["gh"],"credential":"gh"}"#).expect("loads");
        assert_eq!(
            defaults.rules()[0].effective_hosts(),
            vec!["api.github.com", "github.com", "uploads.github.com"]
        );
        // Written back as given (absent fields stay absent).
        let text = serde_json::to_string(&defaults.rules()[0]).expect("json");
        assert_eq!(
            text,
            r#"{"pattern":["gh"],"decision":"allow","credential":"gh"}"#
        );

        for (rule, why) in [
            (
                r#"{"pattern":["gh"],"decision":"prompt","credential":"gh"}"#,
                "allow rule",
            ),
            (
                r#"{"pattern":["gh"],"decision":"forbidden","hosts":["github.com"]}"#,
                "allow rule",
            ),
            (
                r#"{"pattern":["gh"],"decision":"forbidden","conditions":{"forbid_flags":["-f"]}}"#,
                "allow rule",
            ),
            (
                r#"{"pattern":["gh"],"credential":"npm"}"#,
                "not a credential profile",
            ),
            (
                r#"{"pattern":[["gh","hub"]],"credential":"gh"}"#,
                "exactly that word",
            ),
            (
                r#"{"pattern":[{"any":true}],"credential":"gh"}"#,
                "exactly that word",
            ),
            (
                r#"{"pattern":["/opt/homebrew/bin/gh"],"credential":"gh"}"#,
                "exactly that word",
            ),
            (
                r#"{"pattern":["curl"],"credential":"gh"}"#,
                "exactly that word",
            ),
            (
                r#"{"pattern":["gh"],"credential":"gh","network":true}"#,
                "drop `network: true`",
            ),
            (r#"{"pattern":["curl"],"hosts":["10.0.0.1"]}"#, "IP address"),
            (
                r#"{"pattern":["curl"],"hosts":["api.example.com:8443"]}"#,
                "not a host name",
            ),
            (r#"{"pattern":["curl"],"hosts":[""]}"#, "not a host name"),
            (
                r#"{"pattern":["curl"],"conditions":{"forbid_flags":["f"]}}"#,
                "is not a flag",
            ),
            (
                r#"{"pattern":["curl"],"conditions":{"flag_values":{"-X":[]}}}"#,
                "non-empty values",
            ),
        ] {
            let error = loads(rule).expect_err(rule);
            assert!(error.message.contains(why), "{rule}: {}", error.message);
            assert_eq!(error.rule_index, Some(1));
        }
        let many: Vec<String> = (0..33).map(|n| format!("\"h{n}.example.com\"")).collect();
        assert!(
            loads(&format!(
                r#"{{"pattern":["curl"],"hosts":[{}]}}"#,
                many.join(",")
            ))
            .is_err()
        );
        assert!(
            loads(r#"{"pattern":["gh"],"conditions":{"nope":1}}"#).is_err(),
            "an unknown condition refuses the file"
        );
    }

    #[test]
    fn conditions_are_checked_through_the_examples() {
        let refused = loads(
            r#"{"pattern":["gh","api"],
                "conditions":{"flag_values":{"--method":["GET"]}},
                "match":["gh api --method POST x"]}"#,
        )
        .expect_err("the example disagrees");
        assert_eq!(refused.got.as_deref(), Some("did not match"));
    }

    #[test]
    fn same_scope_tells_granted_rules_apart() {
        let plain = PrefixRule::from_command_pattern(vec!["gh".to_owned(), "pr".to_owned()]);
        let mut granted = plain.clone();
        granted.credential = Some("gh".to_owned());
        assert!(!plain.same_scope(&granted));
        let mut hosts = plain.clone();
        hosts.hosts = vec!["github.com".to_owned()];
        assert!(!plain.same_scope(&hosts));
        assert!(!plain.scopes_network() && plain.effective_hosts().is_empty());
    }
}

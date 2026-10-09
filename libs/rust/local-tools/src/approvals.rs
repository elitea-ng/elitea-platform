//! Approval rules: every local tool call is allowed, asked or denied
//! (ADR-0029 decision 4).
//!
//! The layers, in order:
//!
//! 1. **Policy** (`local_work`, decision 6) is a ceiling. Local work off,
//!    shell off, a sandbox wider than `max_sandbox_mode`, network when the
//!    policy denies it, a `command_deny` match in any segment, a command
//!    outside a non-empty `command_allow`, or a `path_deny` path: denied,
//!    and nothing below can change that.
//! 2. **Plan mode**: only read-only tools, until the person accepts the
//!    plan.
//! 3. **Workspace settings**: the person's rules for this folder. A deny
//!    beats an ask beats an allow. The host stores them in its own data
//!    directory, never in the workspace, so a repository cannot grant
//!    itself permissions.
//! 4. **Remembered choices**: "always allow `cargo test` here", recorded
//!    when the person answers `approve_always`. A choice made for one
//!    sandbox mode and network setting never approves a wider one.
//! 5. **Default**: read-only tools are allowed; everything else is asked.
//!
//! A compound command (see [`crate::command`]) is never allowed by layers 3
//! or 4: an allow there becomes an ask.
//!
//! [`RuleApprovals`] exposes the engine as the runtime's
//! [`ApprovalChannel`]: rule verdicts are answered inline, asks go on to the
//! host's prompt channel.

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use elitea_agent_runtime::host::{
    ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError, HostErrorCode,
};
use globset::{Glob, GlobMatcher};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::command::{CommandPattern, CommandShape, analyse};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::workspace::{Intent, Workspace};

/// The payload key a local tool call travels under in an
/// [`ApprovalRequest`]; requests without it (HITL nodes) go straight to the
/// prompt.
pub const PAYLOAD_KEY: &str = "local_tool_call";

/// Approve, and remember the choice for this workspace.
pub const APPROVE_ALWAYS: &str = "approve_always";

/// The local tools, as the rules name them.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolKind {
    ReadFile,
    ListTree,
    SearchFiles,
    ReadDocument,
    GitRead,
    WriteFile,
    EditFile,
    ApplyPatch,
    RunCommand,
}

impl ToolKind {
    /// Whether the tool cannot change anything (what plan mode keeps).
    #[must_use]
    pub const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ReadFile
                | Self::ListTree
                | Self::SearchFiles
                | Self::ReadDocument
                | Self::GitRead
        )
    }
}

/// One tool call, as the rules see it.
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ToolCall {
    pub tool: ToolKind,
    /// Workspace paths it touches (as the tool resolved them).
    #[serde(default)]
    pub paths: Vec<String>,
    /// The command string, for [`ToolKind::RunCommand`].
    #[serde(default)]
    pub command: Option<String>,
    /// The sandbox it asks for, for [`ToolKind::RunCommand`].
    #[serde(default)]
    pub sandbox: Option<SandboxMode>,
    #[serde(default)]
    pub network: bool,
}

impl ToolCall {
    #[must_use]
    pub fn new(tool: ToolKind) -> Self {
        Self {
            tool,
            paths: Vec::new(),
            command: None,
            sandbox: None,
            network: false,
        }
    }

    fn sandbox_mode(&self) -> SandboxMode {
        self.sandbox.unwrap_or(SandboxMode::WorkspaceWrite)
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Verdict {
    Allow,
    Ask,
    Deny,
}

/// Which layer decided.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Source {
    Policy,
    PlanMode,
    Workspace,
    Remembered,
    Default,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Decision {
    pub verdict: Verdict,
    pub source: Source,
    pub reason: String,
}

impl Decision {
    fn new(verdict: Verdict, source: Source, reason: impl Into<String>) -> Self {
        Self {
            verdict,
            source,
            reason: reason.into(),
        }
    }
}

/// One of the person's rules for a workspace.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct WorkspaceRule {
    /// The tools it applies to; empty: all.
    #[serde(default)]
    pub tools: Vec<ToolKind>,
    /// A [`CommandPattern`], for [`ToolKind::RunCommand`].
    #[serde(default)]
    pub command: Option<String>,
    /// A glob over workspace paths: matches when every path the call
    /// touches matches (any path, for a deny).
    #[serde(default)]
    pub path: Option<String>,
    pub verdict: Verdict,
}

/// The person's rules for one workspace.
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct WorkspaceSettings {
    #[serde(default)]
    pub rules: Vec<WorkspaceRule>,
}

/// A remembered "always allow".
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct RememberedChoice {
    pub tool: ToolKind,
    /// The argv prefix, for [`ToolKind::RunCommand`].
    #[serde(default)]
    pub command: Option<Vec<String>>,
    /// The widest sandbox and network the choice was made for.
    #[serde(default)]
    pub sandbox: Option<SandboxMode>,
    #[serde(default)]
    pub network: bool,
}

/// Where remembered choices live (the host's data directory).
pub trait ChoiceStore: Send + Sync {
    fn list(&self) -> Vec<RememberedChoice>;

    /// # Errors
    ///
    /// When the choice cannot be stored.
    fn remember(&self, choice: RememberedChoice) -> ToolResult<()>;
}

/// Choices kept for the life of the process.
#[derive(Debug, Default)]
pub struct MemoryChoices(Mutex<Vec<RememberedChoice>>);

impl ChoiceStore for MemoryChoices {
    fn list(&self) -> Vec<RememberedChoice> {
        self.0
            .lock()
            .map(|choices| choices.clone())
            .unwrap_or_default()
    }

    fn remember(&self, choice: RememberedChoice) -> ToolResult<()> {
        let mut choices = self
            .0
            .lock()
            .map_err(|_| ToolError::new(ErrorCode::Io, "the choice store is unavailable"))?;
        if !choices.contains(&choice) {
            choices.push(choice);
        }
        Ok(())
    }
}

/// Choices kept in one JSON file (one per workspace, in the host's data
/// directory), rewritten atomically.
#[derive(Debug)]
pub struct JsonFileChoices {
    path: PathBuf,
    cache: MemoryChoices,
}

impl JsonFileChoices {
    /// Load `path` (absent: no choices yet).
    ///
    /// # Errors
    ///
    /// When it exists and is not a valid choice file.
    pub fn open(path: PathBuf) -> ToolResult<Self> {
        let cache = MemoryChoices::default();
        match std::fs::read(&path) {
            Ok(bytes) => {
                let choices: Vec<RememberedChoice> = serde_json::from_slice(&bytes)
                    .map_err(|_| ToolError::invalid("the remembered-choices file is not valid"))?;
                for choice in choices {
                    cache.remember(choice)?;
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(ToolError::io("cannot read remembered choices", &error)),
        }
        Ok(Self { path, cache })
    }
}

impl ChoiceStore for JsonFileChoices {
    fn list(&self) -> Vec<RememberedChoice> {
        self.cache.list()
    }

    fn remember(&self, choice: RememberedChoice) -> ToolResult<()> {
        self.cache.remember(choice)?;
        let bytes = serde_json::to_vec_pretty(&self.cache.list())
            .map_err(|_| ToolError::new(ErrorCode::Io, "cannot encode remembered choices"))?;
        if let Some(parent) = self.path.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|error| ToolError::io("cannot store remembered choices", &error))?;
        }
        let temp = self.path.with_extension("json.tmp");
        std::fs::write(&temp, bytes)
            .and_then(|()| std::fs::rename(&temp, &self.path))
            .map_err(|error| ToolError::io("cannot store remembered choices", &error))
    }
}

struct CompiledRule {
    rule: WorkspaceRule,
    command: Option<CommandPattern>,
    path: Option<GlobMatcher>,
}

/// The layered rules for one workspace session.
pub struct RulesEngine {
    policy: LocalWorkPolicy,
    policy_paths: Workspace,
    command_allow: Vec<CommandPattern>,
    command_deny: Vec<CommandPattern>,
    rules: Vec<CompiledRule>,
    choices: Arc<dyn ChoiceStore>,
    plan_mode: AtomicBool,
}

fn patterns(texts: &[String], field: &str) -> ToolResult<Vec<CommandPattern>> {
    texts
        .iter()
        .map(|text| {
            CommandPattern::parse(text).ok_or_else(|| {
                ToolError::invalid(format!("{field} pattern `{text}` is not a command"))
            })
        })
        .collect()
}

impl RulesEngine {
    /// Build the engine for `workspace` (whose root the policy's
    /// `path_deny` is matched under).
    ///
    /// # Errors
    ///
    /// When a pattern or glob does not parse.
    pub fn new(
        policy: LocalWorkPolicy,
        workspace: &Workspace,
        settings: WorkspaceSettings,
        choices: Arc<dyn ChoiceStore>,
    ) -> ToolResult<Self> {
        let rules = settings
            .rules
            .into_iter()
            .map(|rule| {
                let command = match &rule.command {
                    Some(text) => Some(CommandPattern::parse(text).ok_or_else(|| {
                        ToolError::invalid(format!(
                            "workspace rule command `{text}` is not a command"
                        ))
                    })?),
                    None => None,
                };
                let path = match &rule.path {
                    Some(glob) => Some(
                        Glob::new(glob)
                            .map_err(|_| {
                                ToolError::invalid(format!(
                                    "workspace rule path `{glob}` is not a glob"
                                ))
                            })?
                            .compile_matcher(),
                    ),
                    None => None,
                };
                Ok(CompiledRule {
                    rule,
                    command,
                    path,
                })
            })
            .collect::<ToolResult<Vec<_>>>()?;
        Ok(Self {
            command_allow: patterns(&policy.command_allow, "command_allow")?,
            command_deny: patterns(&policy.command_deny, "command_deny")?,
            policy_paths: Workspace::open(workspace.root(), &policy.path_deny)?,
            policy,
            rules,
            choices,
            plan_mode: AtomicBool::new(false),
        })
    }

    #[must_use]
    pub fn policy(&self) -> &LocalWorkPolicy {
        &self.policy
    }

    /// Enter or leave plan mode (read-only tools only).
    pub fn set_plan_mode(&self, on: bool) {
        self.plan_mode.store(on, Ordering::SeqCst);
    }

    #[must_use]
    pub fn plan_mode(&self) -> bool {
        self.plan_mode.load(Ordering::SeqCst)
    }

    /// Decide one call.
    #[must_use]
    pub fn decide(&self, call: &ToolCall) -> Decision {
        if let Some(decision) = self.policy_ceiling(call) {
            return decision;
        }
        if self.plan_mode() && !call.tool.is_read_only() {
            return Decision::new(
                Verdict::Deny,
                Source::PlanMode,
                "plan mode allows read-only tools until the plan is accepted",
            );
        }
        let shape = call.command.as_deref().map(analyse);
        if let Some(decision) = self.workspace_rules(call, shape.as_ref()) {
            return decision;
        }
        let simple = shape.as_ref().is_none_or(CommandShape::is_simple);
        if simple && self.remembered(call, shape.as_ref()) {
            return Decision::new(
                Verdict::Allow,
                Source::Remembered,
                "you chose to always allow this here",
            );
        }
        if call.tool.is_read_only() {
            Decision::new(
                Verdict::Allow,
                Source::Default,
                "read-only tools are allowed",
            )
        } else {
            let reason = match &shape {
                Some(CommandShape::Compound { reason, .. }) => {
                    format!("compound command ({reason})")
                }
                _ => "changes need your approval".to_owned(),
            };
            Decision::new(Verdict::Ask, Source::Default, reason)
        }
    }

    fn policy_ceiling(&self, call: &ToolCall) -> Option<Decision> {
        let deny = |reason: String| Some(Decision::new(Verdict::Deny, Source::Policy, reason));
        if !self.policy.allowed {
            return deny("local work is disabled by policy".to_owned());
        }
        for path in &call.paths {
            let denied = self
                .policy_paths
                .resolve(path, Intent::Read)
                .err()
                .filter(|error| error.code() != ErrorCode::InvalidArgument);
            if let Some(error) = denied {
                return deny(error.message().to_owned());
            }
        }
        if call.tool != ToolKind::RunCommand {
            return None;
        }
        if !self.policy.shell {
            return deny("the shell is disabled by policy".to_owned());
        }
        if call.sandbox_mode() > self.policy.max_sandbox_mode {
            return deny(format!(
                "policy allows at most the {} sandbox",
                self.policy.max_sandbox_mode.as_str()
            ));
        }
        if call.network && !self.policy.network {
            return deny("network access is disabled by policy".to_owned());
        }
        let Some(command) = call.command.as_deref() else {
            return deny("no command".to_owned());
        };
        let shape = analyse(command);
        let segments = shape.segments();
        if segments.is_empty() {
            return deny("the command cannot be analysed".to_owned());
        }
        if let Some(argv) = segments.iter().find(|argv| {
            self.command_deny
                .iter()
                .any(|pattern| pattern.matches(argv))
        }) {
            return deny(format!("`{}` is denied by policy", argv.join(" ")));
        }
        if !self.command_allow.is_empty()
            && let Some(argv) = segments.iter().find(|argv| {
                !self
                    .command_allow
                    .iter()
                    .any(|pattern| pattern.matches(argv))
            })
        {
            return deny(format!(
                "`{}` is not in the policy's command_allow",
                argv.join(" ")
            ));
        }
        None
    }

    fn workspace_rules(&self, call: &ToolCall, shape: Option<&CommandShape>) -> Option<Decision> {
        let segments = shape.map(CommandShape::segments).unwrap_or_default();
        let mut best: Option<&CompiledRule> = None;
        for compiled in &self.rules {
            let rule = &compiled.rule;
            if !rule.tools.is_empty() && !rule.tools.contains(&call.tool) {
                continue;
            }
            let deny = rule.verdict == Verdict::Deny;
            if let Some(pattern) = &compiled.command {
                // Deny: any segment. Allow/ask: the command itself.
                let hit = if deny {
                    segments.iter().any(|argv| pattern.matches(argv))
                } else {
                    segments.first().is_some_and(|argv| pattern.matches(argv))
                };
                if !hit {
                    continue;
                }
            }
            if let Some(glob) = &compiled.path {
                let hit = if deny {
                    call.paths.iter().any(|path| glob.is_match(path))
                } else {
                    !call.paths.is_empty() && call.paths.iter().all(|path| glob.is_match(path))
                };
                if !hit {
                    continue;
                }
            }
            let rank = |verdict: Verdict| match verdict {
                Verdict::Deny => 2,
                Verdict::Ask => 1,
                Verdict::Allow => 0,
            };
            if best.is_none_or(|current| rank(rule.verdict) > rank(current.rule.verdict)) {
                best = Some(compiled);
            }
        }
        let rule = &best?.rule;
        let compound = shape.is_some_and(|shape| !shape.is_simple());
        Some(match rule.verdict {
            Verdict::Allow if compound => Decision::new(
                Verdict::Ask,
                Source::Workspace,
                "a workspace rule allows it, but compound commands are always asked",
            ),
            verdict => Decision::new(verdict, Source::Workspace, "a workspace rule"),
        })
    }

    fn remembered(&self, call: &ToolCall, shape: Option<&CommandShape>) -> bool {
        let argv = match shape {
            Some(CommandShape::Simple(argv)) => Some(argv),
            Some(CommandShape::Compound { .. }) => return false,
            None => None,
        };
        self.choices.list().iter().any(|choice| {
            choice.tool == call.tool
                && call.sandbox_mode() <= choice.sandbox.unwrap_or(SandboxMode::WorkspaceWrite)
                && (!call.network || choice.network)
                && match (&choice.command, argv) {
                    (Some(prefix), Some(argv)) => CommandPattern::from_tokens(prefix.clone())
                        .is_some_and(|pattern| pattern.matches(argv)),
                    (None, None) => true,
                    _ => false,
                }
        })
    }

    /// Remember an "always allow" for `call`. For a command, `prefix`
    /// narrows or names the argv prefix (`cargo test` for
    /// `cargo test --all`); it must be a prefix of the command. Compound
    /// commands are never remembered.
    ///
    /// # Errors
    ///
    /// When the choice is not rememberable or cannot be stored.
    pub fn remember(&self, call: &ToolCall, prefix: Option<&str>) -> ToolResult<()> {
        let command = match call.command.as_deref().map(analyse) {
            None => None,
            Some(CommandShape::Simple(argv)) => {
                let tokens = match prefix.and_then(CommandPattern::parse) {
                    Some(pattern) if pattern.matches(&argv) => pattern.tokens().to_vec(),
                    Some(_) => {
                        return Err(ToolError::invalid(
                            "the remembered prefix does not match the command",
                        ));
                    }
                    None => argv,
                };
                Some(tokens)
            }
            Some(CommandShape::Compound { .. }) => {
                return Err(ToolError::new(
                    ErrorCode::Denied,
                    "compound commands are asked every time",
                ));
            }
        };
        self.choices.remember(RememberedChoice {
            tool: call.tool,
            command,
            sandbox: call.sandbox,
            network: call.network,
        })
    }
}

/// A prompt channel for hosts with no UI (tests, headless runs): every
/// question is deferred.
#[derive(Debug, Default)]
pub struct DeferringPrompt;

#[async_trait]
impl ApprovalChannel for DeferringPrompt {
    async fn request(&self, _request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        Ok(ApprovalOutcome::Deferred)
    }
}

/// The rules engine as the runtime's [`ApprovalChannel`].
pub struct RuleApprovals {
    engine: Arc<RulesEngine>,
    prompt: Arc<dyn ApprovalChannel>,
}

impl RuleApprovals {
    /// Rules first; what they ask goes to `prompt` (the host's UI).
    #[must_use]
    pub fn new(engine: Arc<RulesEngine>, prompt: Arc<dyn ApprovalChannel>) -> Self {
        Self { engine, prompt }
    }

    #[must_use]
    pub fn engine(&self) -> &Arc<RulesEngine> {
        &self.engine
    }
}

fn decided(action: &str, decision: &Decision) -> ApprovalOutcome {
    ApprovalOutcome::Decided {
        action: action.to_owned(),
        value: json!({ "source": decision.source, "reason": decision.reason }),
    }
}

#[async_trait]
impl ApprovalChannel for RuleApprovals {
    async fn request(&self, mut request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        let call = request
            .payload
            .get(PAYLOAD_KEY)
            .cloned()
            .and_then(|value| serde_json::from_value::<ToolCall>(value).ok());
        let Some(call) = call else {
            return self.prompt.request(request).await;
        };
        let decision = self.engine.decide(&call);
        match decision.verdict {
            Verdict::Allow => Ok(decided("approve", &decision)),
            Verdict::Deny => Ok(decided("reject", &decision)),
            Verdict::Ask => {
                let rememberable = call
                    .command
                    .as_deref()
                    .is_none_or(|command| analyse(command).is_simple());
                request.available_actions = if rememberable {
                    vec![
                        "approve".to_owned(),
                        APPROVE_ALWAYS.to_owned(),
                        "reject".to_owned(),
                    ]
                } else {
                    vec!["approve".to_owned(), "reject".to_owned()]
                };
                if let Value::Object(payload) = &mut request.payload {
                    payload.insert("reason".to_owned(), Value::String(decision.reason.clone()));
                }
                let outcome = self.prompt.request(request).await?;
                match outcome {
                    ApprovalOutcome::Decided { action, value } if action == APPROVE_ALWAYS => {
                        if rememberable {
                            let prefix = value.get("prefix").and_then(Value::as_str);
                            self.engine.remember(&call, prefix).map_err(|_| {
                                HostError::new(
                                    HostErrorCode::InvalidInput,
                                    "the choice could not be remembered",
                                )
                            })?;
                        }
                        Ok(ApprovalOutcome::Decided {
                            action: "approve".to_owned(),
                            value,
                        })
                    }
                    other => Ok(other),
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use async_trait::async_trait;
    use elitea_agent_runtime::host::{
        ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError,
    };
    use serde_json::json;

    use super::{
        ChoiceStore, JsonFileChoices, MemoryChoices, PAYLOAD_KEY, RuleApprovals, RulesEngine,
        Source, ToolCall, ToolKind, Verdict, WorkspaceRule, WorkspaceSettings,
    };
    use crate::policy::{LocalWorkPolicy, SandboxMode};
    use crate::workspace::Workspace;

    fn policy() -> LocalWorkPolicy {
        LocalWorkPolicy {
            allowed: true,
            shell: true,
            max_sandbox_mode: SandboxMode::WorkspaceWrite,
            network: false,
            command_allow: Vec::new(),
            command_deny: vec!["rm".to_owned(), "git push".to_owned()],
            path_deny: vec!["*.pem".to_owned()],
        }
    }

    fn engine_with(
        policy: LocalWorkPolicy,
        rules: Vec<WorkspaceRule>,
    ) -> (tempfile::TempDir, RulesEngine) {
        let dir = tempfile::tempdir().expect("dir");
        let workspace = Workspace::open(dir.path(), &[]).expect("workspace");
        let engine = RulesEngine::new(
            policy,
            &workspace,
            WorkspaceSettings { rules },
            Arc::new(MemoryChoices::default()),
        )
        .expect("engine");
        (dir, engine)
    }

    fn shell(command: &str) -> ToolCall {
        ToolCall {
            command: Some(command.to_owned()),
            ..ToolCall::new(ToolKind::RunCommand)
        }
    }

    fn file(tool: ToolKind, path: &str) -> ToolCall {
        ToolCall {
            paths: vec![path.to_owned()],
            ..ToolCall::new(tool)
        }
    }

    fn verdict(engine: &RulesEngine, call: &ToolCall) -> (Verdict, Source) {
        let decision = engine.decide(call);
        (decision.verdict, decision.source)
    }

    #[test]
    fn the_policy_is_a_ceiling() {
        let (_dir, engine) = engine_with(policy(), Vec::new());
        assert_eq!(
            verdict(&engine, &shell("rm -rf build")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("/bin/rm x")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("git push origin main")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("cargo build && rm -rf /")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("bash -c 'rm -rf /'")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("sudo -u root rm x")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::ReadFile, "keys/id.pem")),
            (Verdict::Deny, Source::Policy)
        );
        let full = ToolCall {
            sandbox: Some(SandboxMode::FullAccess),
            ..shell("cargo test")
        };
        assert_eq!(verdict(&engine, &full), (Verdict::Deny, Source::Policy));
        let network = ToolCall {
            network: true,
            ..shell("cargo test")
        };
        assert_eq!(verdict(&engine, &network), (Verdict::Deny, Source::Policy));

        let (_dir, off) = engine_with(LocalWorkPolicy::default(), Vec::new());
        assert_eq!(
            verdict(&off, &file(ToolKind::ReadFile, "a")),
            (Verdict::Deny, Source::Policy)
        );
        let no_shell = LocalWorkPolicy {
            shell: false,
            ..policy()
        };
        let (_dir, no_shell) = engine_with(no_shell, Vec::new());
        assert_eq!(
            verdict(&no_shell, &shell("ls")),
            (Verdict::Deny, Source::Policy)
        );
    }

    #[test]
    fn a_policy_allow_list_admits_only_its_commands_and_still_asks() {
        let allow_list = LocalWorkPolicy {
            command_allow: vec!["cargo".to_owned(), "git status".to_owned()],
            ..policy()
        };
        let (_dir, engine) = engine_with(allow_list, Vec::new());
        assert_eq!(
            verdict(&engine, &shell("cargo test")),
            (Verdict::Ask, Source::Default)
        );
        assert_eq!(
            verdict(&engine, &shell("git status")),
            (Verdict::Ask, Source::Default)
        );
        assert_eq!(
            verdict(&engine, &shell("git commit")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("cargo test | tee x")),
            (Verdict::Deny, Source::Policy)
        );
        assert_eq!(
            verdict(&engine, &shell("cargo test && cargo build")),
            (Verdict::Ask, Source::Default)
        );
    }

    #[test]
    fn defaults_allow_reads_and_ask_for_changes() {
        let (_dir, engine) = engine_with(policy(), Vec::new());
        for tool in [
            ToolKind::ReadFile,
            ToolKind::ListTree,
            ToolKind::SearchFiles,
            ToolKind::ReadDocument,
            ToolKind::GitRead,
        ] {
            assert_eq!(
                verdict(&engine, &file(tool, "src/lib.rs")),
                (Verdict::Allow, Source::Default),
                "{tool:?}"
            );
        }
        for tool in [
            ToolKind::WriteFile,
            ToolKind::EditFile,
            ToolKind::ApplyPatch,
        ] {
            assert_eq!(
                verdict(&engine, &file(tool, "src/lib.rs")),
                (Verdict::Ask, Source::Default),
                "{tool:?}"
            );
        }
        assert_eq!(
            verdict(&engine, &shell("cargo test")),
            (Verdict::Ask, Source::Default)
        );
    }

    #[test]
    fn plan_mode_denies_every_change() {
        let (_dir, engine) = engine_with(
            policy(),
            vec![WorkspaceRule {
                tools: Vec::new(),
                command: None,
                path: None,
                verdict: Verdict::Allow,
            }],
        );
        engine.set_plan_mode(true);
        assert_eq!(
            verdict(&engine, &file(ToolKind::WriteFile, "a")),
            (Verdict::Deny, Source::PlanMode)
        );
        assert_eq!(
            verdict(&engine, &shell("ls")),
            (Verdict::Deny, Source::PlanMode)
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::ReadFile, "a")).0,
            Verdict::Allow
        );
        engine.set_plan_mode(false);
        assert_eq!(
            verdict(&engine, &file(ToolKind::WriteFile, "a")),
            (Verdict::Allow, Source::Workspace)
        );
    }

    #[test]
    fn workspace_rules_rank_deny_over_ask_over_allow_and_never_allow_compounds() {
        let rules = vec![
            WorkspaceRule {
                tools: vec![ToolKind::RunCommand],
                command: Some("cargo".to_owned()),
                path: None,
                verdict: Verdict::Allow,
            },
            WorkspaceRule {
                tools: vec![ToolKind::RunCommand],
                command: Some("cargo publish".to_owned()),
                path: None,
                verdict: Verdict::Deny,
            },
            WorkspaceRule {
                tools: vec![ToolKind::WriteFile, ToolKind::EditFile],
                command: None,
                path: Some("docs/**".to_owned()),
                verdict: Verdict::Allow,
            },
            WorkspaceRule {
                tools: vec![ToolKind::ReadFile],
                command: None,
                path: Some("private/**".to_owned()),
                verdict: Verdict::Ask,
            },
        ];
        let (_dir, engine) = engine_with(policy(), rules);
        assert_eq!(
            verdict(&engine, &shell("cargo test")),
            (Verdict::Allow, Source::Workspace)
        );
        assert_eq!(
            verdict(&engine, &shell("cargo publish")),
            (Verdict::Deny, Source::Workspace)
        );
        assert_eq!(
            verdict(&engine, &shell("cargo test | tee log")),
            (Verdict::Ask, Source::Workspace)
        );
        assert_eq!(
            verdict(&engine, &shell("ls && cargo publish")),
            (Verdict::Deny, Source::Workspace)
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::EditFile, "docs/a.md")),
            (Verdict::Allow, Source::Workspace)
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::EditFile, "src/a.rs")),
            (Verdict::Ask, Source::Default)
        );
        let mixed = ToolCall {
            paths: vec!["docs/a.md".to_owned(), "src/a.rs".to_owned()],
            ..ToolCall::new(ToolKind::WriteFile)
        };
        assert_eq!(
            verdict(&engine, &mixed),
            (Verdict::Ask, Source::Default),
            "an allow needs every path"
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::ReadFile, "private/x")),
            (Verdict::Ask, Source::Workspace)
        );
    }

    #[test]
    fn remembered_choices_allow_only_within_their_scope() {
        let (_dir, engine) = engine_with(policy(), Vec::new());
        engine
            .remember(&shell("cargo test --all"), Some("cargo test"))
            .expect("remember");
        assert_eq!(
            verdict(&engine, &shell("cargo test -p x")),
            (Verdict::Allow, Source::Remembered)
        );
        assert_eq!(verdict(&engine, &shell("cargo build")).0, Verdict::Ask);
        assert_eq!(
            verdict(&engine, &shell("cargo test; curl evil")).0,
            Verdict::Ask
        );
        let read_only = ToolCall {
            sandbox: Some(SandboxMode::ReadOnly),
            ..shell("cargo test")
        };
        assert_eq!(
            verdict(&engine, &read_only).0,
            Verdict::Allow,
            "narrower sandbox is covered"
        );
        assert!(
            engine.remember(&shell("cargo test | tee"), None).is_err(),
            "compounds are not remembered"
        );
        assert!(
            engine
                .remember(&shell("cargo test"), Some("cargo build"))
                .is_err()
        );
        engine
            .remember(&file(ToolKind::EditFile, "a"), None)
            .expect("remember edits");
        assert_eq!(
            verdict(&engine, &file(ToolKind::EditFile, "b")),
            (Verdict::Allow, Source::Remembered)
        );
        assert_eq!(
            verdict(&engine, &file(ToolKind::WriteFile, "b")).0,
            Verdict::Ask
        );
    }

    #[test]
    fn json_file_choices_survive_a_restart() {
        let dir = tempfile::tempdir().expect("dir");
        let path = dir.path().join("nested/choices.json");
        let store = JsonFileChoices::open(path.clone()).expect("open");
        store
            .remember(super::RememberedChoice {
                tool: ToolKind::RunCommand,
                command: Some(vec!["cargo".to_owned(), "test".to_owned()]),
                sandbox: None,
                network: false,
            })
            .expect("remember");
        let reopened = JsonFileChoices::open(path).expect("reopen");
        assert_eq!(reopened.list().len(), 1);
    }

    struct ScriptedPrompt {
        answer: ApprovalOutcome,
        seen: Mutex<Vec<ApprovalRequest>>,
    }

    #[async_trait]
    impl ApprovalChannel for ScriptedPrompt {
        async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
            self.seen.lock().expect("lock").push(request);
            Ok(self.answer.clone())
        }
    }

    fn request(call: &ToolCall) -> ApprovalRequest {
        ApprovalRequest {
            subject: "call-1".to_owned(),
            message: "run".to_owned(),
            available_actions: vec!["approve".to_owned(), "reject".to_owned()],
            payload: json!({ PAYLOAD_KEY: call }),
        }
    }

    #[tokio::test]
    async fn the_channel_answers_rules_inline_and_remembers_approve_always() {
        let (_dir, engine) = engine_with(policy(), Vec::new());
        let engine = Arc::new(engine);
        let prompt = Arc::new(ScriptedPrompt {
            answer: ApprovalOutcome::Decided {
                action: super::APPROVE_ALWAYS.to_owned(),
                value: json!({ "prefix": "cargo test" }),
            },
            seen: Mutex::new(Vec::new()),
        });
        let channel = RuleApprovals::new(engine.clone(), prompt.clone());
        let read = channel
            .request(request(&file(ToolKind::ReadFile, "a")))
            .await
            .expect("read");
        assert!(matches!(read, ApprovalOutcome::Decided { ref action, .. } if action == "approve"));
        let denied = channel
            .request(request(&shell("rm x")))
            .await
            .expect("deny");
        assert!(
            matches!(denied, ApprovalOutcome::Decided { ref action, .. } if action == "reject")
        );
        assert!(
            prompt.seen.lock().expect("lock").is_empty(),
            "rules answered without the prompt"
        );

        let asked = channel
            .request(request(&shell("cargo test --all")))
            .await
            .expect("ask");
        assert!(
            matches!(asked, ApprovalOutcome::Decided { ref action, .. } if action == "approve")
        );
        let seen = prompt.seen.lock().expect("lock").clone();
        assert_eq!(seen.len(), 1);
        assert!(
            seen[0]
                .available_actions
                .contains(&super::APPROVE_ALWAYS.to_owned())
        );
        assert_eq!(
            engine.decide(&shell("cargo test -q")).source,
            Source::Remembered
        );

        let compound = channel
            .request(request(&shell("cargo test | tee x")))
            .await
            .expect("compound");
        assert!(
            matches!(compound, ApprovalOutcome::Decided { ref action, .. } if action == "approve")
        );
        let seen = prompt.seen.lock().expect("lock").clone();
        assert!(
            !seen[1]
                .available_actions
                .contains(&super::APPROVE_ALWAYS.to_owned()),
            "compounds cannot be remembered"
        );

        let hitl = ApprovalRequest {
            payload: json!({ "node": "review" }),
            ..request(&shell("ls"))
        };
        channel.request(hitl).await.expect("hitl");
        assert_eq!(
            prompt.seen.lock().expect("lock").len(),
            3,
            "non-tool requests pass through"
        );
    }
}

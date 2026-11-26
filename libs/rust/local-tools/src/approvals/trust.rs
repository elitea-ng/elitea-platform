//! Approvals in a trusted folder (ADR-0061 decisions 5d–5g).
//!
//! A command in a folder the person trusted runs on the host
//! ([`ToolCall::host`], [`crate::sandbox::SandboxRequest::host`]): nothing
//! confines it after it starts, so the decisions stay in front of it. The
//! layers keep their order:
//!
//! | Layer | In a trusted folder |
//! |---|---|
//! | 1. Policy ceiling | unchanged and first (`command_deny` in every segment, `command_allow`, the shell, `path_deny`) |
//! | 2. Hooks, plan mode | unchanged: plan mode denies a command; a hook's deny wins and its ask asks |
//! | 3. Forbidden and prompt rules, every origin | unchanged: forbidden denies, prompt asks |
//! | "Ask for every command" | every host command left is asked (with the taint gate's source after an untrusted read); no rule, remembered choice or default allows one |
//! | 4. An allow rule the **person** wrote (user rules; workspace rules without `origin: repository`; not an older host's) | allows a host command without a question when the rule says `sandbox: host` or names no sandbox, and grants nothing (`credential`, `hosts`: those keep their sandbox, decision 5h). It still answers after an untrusted read |
//! | 5. "Always allow" made in a trusted folder (`sandbox: host`, `requires_trust: true`) | allows it, unless the turn read untrusted content |
//! | 6. Defaults | allowed only when the classifier rates it routine, it starts no network-capable program ([`crate::classify::network_program`]), and the turn read nothing untrusted. Everything else is asked. The program list is a best effort: a build or package script (`npm run`, `cargo build`) may still reach the network |
//! | Repository rules | never allow a host run; they can still forbid or ask |
//!
//! **Untrusted input closes the gate (5f).** The engine keeps an
//! [`UntrustedReads`] per turn: the host records a read when a remote
//! toolkit, MCP, web or browser tool returns, a cloud member answers, a
//! hand-off message or consult reply arrives (such a turn starts with it
//! recorded: [`RulesEngine::reset_untrusted_reads`]). After that, every host
//! command not covered by layer 4 is asked with [`TAINT_REASON`]
//! ([`Source::Taint`]). Bypass does not answer that ask, and neither does the
//! unattended grant (5g): only the person, or an allow rule they wrote.
//!
//! In an untrusted folder nothing here applies: [`ToolCall::host`] is never
//! set, and every unconfined command is asked as before.

use std::sync::Arc;

use elitea_agent_runtime::untrusted::UntrustedReads;

use super::{
    Decision, KEEP_ALIVE_REASON, RuleMatch, RulesEngine, Source, ToolCall, ToolKind, Verdict,
};
use crate::classify::{Place, Risk, assess, network_program};
use crate::command::CommandShape;
use crate::error::{ErrorCode, ToolError};
use crate::execpolicy::{PrefixRule, RuleDecision, RuleOrigin, RuleScope};

/// Why a host command is asked after the turn read untrusted content.
pub const TAINT_REASON: &str = "after reading content from outside this folder, a command \
     that runs outside the sandbox is asked (only you, an allow rule you wrote, or Full auto can answer \
     it; bypass mode does not)";

/// Why every host command is asked in a folder that asks for every command.
pub const ASK_EVERY_COMMAND_REASON: &str =
    "this trusted folder asks for every command that runs outside the sandbox";

/// What the host knows about a trusted folder's settings.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct FolderTrust {
    /// The folder's "Ask for every command" switch: no host command is
    /// allowed without a question.
    pub ask_every_command: bool,
}

impl RulesEngine {
    /// Set the folder's trust for the commands decided from now on (`None`:
    /// not trusted, or suspended). The host sets it at a turn's start and
    /// clears it the moment trust is revoked or suspended.
    pub fn set_folder_trust(&self, trust: Option<FolderTrust>) {
        if let Ok(mut current) = self.trust.write() {
            *current = trust;
        }
    }

    /// The folder's trust as the host set it.
    #[must_use]
    pub fn folder_trust(&self) -> Option<FolderTrust> {
        self.trust.read().ok().and_then(|trust| *trust)
    }

    /// Whether the folder is trusted and the policy still allows trust
    /// (checked again here: a policy that stopped allowing it wins at once).
    #[must_use]
    pub fn folder_trusted(&self) -> bool {
        self.folder_trust().is_some() && self.policy.folder_trust_allowed()
    }

    /// Start a turn's record of untrusted reads: `tainted` when its input
    /// came from another agent (a hand-off or a consult, decision 5f), so
    /// the gate starts closed.
    pub fn reset_untrusted_reads(&self, tainted: bool) {
        let reads = UntrustedReads::new(true);
        if tainted {
            reads.record_read();
        }
        if let Ok(mut current) = self.reads.write() {
            *current = reads;
        }
    }

    /// Record that the running turn read untrusted content.
    pub fn record_untrusted_read(&self) {
        if let Ok(reads) = self.reads.read() {
            reads.record_read();
        }
    }

    /// Whether the running turn read untrusted content.
    #[must_use]
    pub fn read_untrusted(&self) -> bool {
        self.reads.read().is_ok_and(|reads| reads.has_read())
    }

    /// The running turn's record (shared with a host that records reads
    /// itself).
    #[must_use]
    pub fn untrusted_reads(&self) -> Arc<UntrustedReads> {
        self.reads.read().map_or_else(
            // A poisoned lock reads as tainted: the gate fails closed.
            |_| UntrustedReads::new(true),
            |reads| Arc::clone(&reads),
        )
    }

    /// Whether `call` runs on the host here: it says so, it runs commands,
    /// and the folder is trusted now.
    pub(super) fn on_host(&self, call: &ToolCall) -> bool {
        call.host
            && (call.tool.runs_commands() || call.tool == ToolKind::ProcessWrite)
            && self.folder_trusted()
    }

    /// Layers 3 to 6 for a host command (the module table). Layers 1 and 2
    /// were decided before.
    pub(super) fn decide_host(&self, call: &ToolCall, shape: Option<&CommandShape>) -> Decision {
        if call.tool == ToolKind::ProcessWrite {
            return self.decide_host_input(call);
        }
        // The file tools' (older) path rules only tighten here.
        if let Some(path) = self.path_rules(call, shape, None)
            && path.verdict != Verdict::Allow
        {
            return path;
        }
        let winner = shape.and_then(|shape| self.winning_host_rule(call, shape));
        if let Some((scope, index, rule)) = &winner
            && rule.decision.tightens()
        {
            return Self::host_rule_decision(*scope, *index, rule);
        }
        if call.keep_alive {
            return Decision::new(Verdict::Ask, Source::Default, KEEP_ALIVE_REASON);
        }
        // "Ask for every command": no allow answers, the taint gate's ask
        // keeps its source (bypass never answers it).
        if self
            .folder_trust()
            .is_some_and(|trust| trust.ask_every_command)
        {
            if self.read_untrusted() {
                return Decision::new(Verdict::Ask, Source::Taint, TAINT_REASON);
            }
            return Decision::new(Verdict::Ask, Source::Default, ASK_EVERY_COMMAND_REASON);
        }
        let allow = winner.filter(|(_, _, rule)| rule.decision == RuleDecision::Allow);
        // Layer 4: a rule the person wrote answers, even after an untrusted
        // read (the person vouched for this command themselves).
        if let Some((scope, index, rule)) =
            allow.as_ref().filter(|(_, _, rule)| !rule.requires_trust)
        {
            return Self::host_rule_decision(*scope, *index, rule);
        }
        // Decision 5f: neither a remembered choice nor the defaults answer
        // once the turn read something from outside the folder.
        if self.read_untrusted() {
            return Decision::new(Verdict::Ask, Source::Taint, TAINT_REASON);
        }
        // Layer 5: "Always allow" made in a trusted folder.
        if let Some((scope, index, rule)) = &allow {
            let mut decision = Self::host_rule_decision(*scope, *index, rule);
            decision.source = Source::Remembered;
            return decision;
        }
        // Layer 6: the defaults.
        let (cwd, _) = self.resolution(call);
        let command = call.command.as_deref().unwrap_or_default();
        match assess(
            command,
            Place {
                root: self.policy_paths.root(),
                cwd: &cwd,
            },
        ) {
            Risk::Destructive(reason) => Decision::new(
                Verdict::Ask,
                Source::Default,
                format!("{reason} (it would run outside the sandbox)"),
            ),
            Risk::Routine => match shape.and_then(network_program) {
                Some(program) => Decision::new(
                    Verdict::Ask,
                    Source::Default,
                    format!(
                        "`{program}` can reach the network, and this command would run outside \
                         the sandbox"
                    ),
                ),
                None => Decision::new(
                    Verdict::Allow,
                    Source::Default,
                    "a routine command, run outside the sandbox in this trusted folder",
                ),
            },
        }
    }

    /// Text typed into a process that runs on the host: as the defaults
    /// judge it, but asked after an untrusted read (a shell on the host is
    /// a host command).
    fn decide_host_input(&self, call: &ToolCall) -> Decision {
        if self.read_untrusted() {
            return Decision::new(Verdict::Ask, Source::Taint, TAINT_REASON);
        }
        match self.input_risk(call) {
            Risk::Destructive(reason) => Decision::new(Verdict::Ask, Source::Default, reason),
            Risk::Routine => Decision::new(
                Verdict::Allow,
                Source::Default,
                "input to the session's own process",
            ),
        }
    }

    /// The decision a prefix rule makes of a host command, as the rules
    /// panel and the model see it.
    fn host_rule_decision(scope: RuleScope, index: usize, rule: &PrefixRule) -> Decision {
        let verdict = match rule.decision {
            RuleDecision::Allow => Verdict::Allow,
            RuleDecision::Prompt => Verdict::Ask,
            RuleDecision::Forbidden => Verdict::Deny,
        };
        let mut reason = match (&rule.justification, rule.origin) {
            (Some(justification), None) => justification.clone(),
            (_, Some(RuleOrigin::Repository)) => format!(
                "{} rule {index} (imported from the repository)",
                scope.as_str()
            ),
            (None, None) => format!("{} rule {index}", scope.as_str()),
        };
        if verdict == Verdict::Allow {
            reason.push_str(" (runs outside the sandbox in this trusted folder)");
        }
        Decision {
            verdict,
            source: scope.into(),
            reason,
            rule: Some(RuleMatch {
                scope,
                index,
                decision: rule.decision,
                pattern: rule.display_pattern(),
                justification: rule.justification.clone(),
                origin: rule.origin,
            }),
        }
    }

    /// Whether an allow `rule` of `scope` may answer a host command: the
    /// person wrote it (not a repository's, not an older host's settings,
    /// not an "Always allow" made in the sandbox), it says `sandbox: host`
    /// or names no sandbox, and it grants nothing
    /// (a grant's command keeps its sandbox, decision 5h; where the grant
    /// cannot run, such a rule stays inert, as on Linux and Windows).
    pub(super) fn allows_host(scope: RuleScope, rule: &PrefixRule) -> bool {
        rule.decision == RuleDecision::Allow
            && rule.trusted()
            && scope != RuleScope::Legacy
            // "Always allow" made outside trust vouched for the sandbox.
            && (!rule.is_remembered() || rule.requires_trust)
            && rule.sandbox.is_none_or(crate::execpolicy::RuleSandbox::is_host)
            && !rule.scopes_network()
    }

    /// Why "always allow" is not offered for a host command, or `None`: a
    /// compound command, a destructive one, or a turn that read untrusted
    /// content (decision 5e) are asked each time.
    pub(super) fn host_never_remembered(&self, call: &ToolCall) -> Option<ToolError> {
        let command = call.command.as_deref()?;
        if !crate::command::analyse(command).is_simple() {
            return Some(super::compound_refusal());
        }
        if self.read_untrusted() {
            return Some(ToolError::new(
                ErrorCode::Denied,
                "after reading content from outside this folder, \"always allow\" is not offered",
            ));
        }
        let (cwd, _) = self.resolution(call);
        let place = Place {
            root: self.policy_paths.root(),
            cwd: &cwd,
        };
        if let Risk::Destructive(reason) = assess(command, place) {
            return Some(ToolError::new(
                ErrorCode::Denied,
                format!("{reason}: asked every time outside the sandbox"),
            ));
        }
        None
    }

    /// The prefix rule that decides a host command: the strongest of the
    /// forbidden and prompt rules matching any segment (every origin), else
    /// an allow [`Self::allows_host`] admits matching the simple command.
    fn winning_host_rule(
        &self,
        call: &ToolCall,
        shape: &CommandShape,
    ) -> Option<(RuleScope, usize, PrefixRule)> {
        self.winning_prefix_rule_by(call, shape, &|scope, rule, _argv| {
            Self::allows_host(scope, rule)
        })
    }
}

#[cfg(test)]
mod tests;

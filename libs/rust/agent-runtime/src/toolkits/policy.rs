use std::collections::{BTreeMap, HashMap, HashSet};
use std::fmt;
use std::sync::Arc;

use serde_json::{Map, Value};

use crate::approval_policy::{ApprovalPolicy, ApprovalRule};
use crate::egress::RunEgress;
use crate::preamble::ContentControls;
use crate::run_caps::RunCapsMeter;
use crate::tool_category::tool_category;

const MAX_POLICY_IDENTIFIERS: usize = 16_384;
const MAX_POLICY_IDENTIFIER_BYTES: usize = 1_024;
const MAX_POLICY_COMPANY_BYTES: usize = 1_024;
const MAX_POLICY_TEMPLATE_BYTES: usize = 8 * 1_024;
const MAX_POLICY_MESSAGE_BYTES: usize = 16 * 1_024;
const DEFAULT_COMPANY_NAME: &str = "Your organization";
const DEFAULT_MESSAGE_TEMPLATE: &str =
    "{company_name} requires approval before running the sensitive action '{action_name}'.";

/// Stable result of evaluating one toolkit or concrete tool.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolAdmissionDecision {
    Allowed,
    BlockedToolkit,
    BlockedTool,
    /// An administrator suspended the toolkit type (ADR-0041 decision 1, the
    /// `toolkit_type` kill switch scope). It refuses like a blocked toolkit,
    /// and the call-time refusal names `execution_suspended`.
    SuspendedToolkit,
}

/// Stable, data-free configuration failure for one policy generation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolAdmissionPolicyErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// An over-limit immutable toolkit policy.
pub struct ToolAdmissionPolicyError {
    code: ToolAdmissionPolicyErrorCode,
}

impl ToolAdmissionPolicyError {
    #[must_use]
    pub const fn code(&self) -> ToolAdmissionPolicyErrorCode {
        self.code
    }
}

impl fmt::Debug for ToolAdmissionPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolAdmissionPolicyError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ToolAdmissionPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            ToolAdmissionPolicyErrorCode::InvalidConfiguration => {
                "the toolkit security policy is malformed"
            }
            ToolAdmissionPolicyErrorCode::ResourceExhausted => {
                "the toolkit admission policy exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ToolAdmissionPolicyError {}

/// One immutable generation of platform toolkit restrictions.
///
/// The outer worker runtime may replace an `Arc<ToolAdmissionPolicy>` when its
/// trusted deployment configuration changes. An in-flight invocation retains
/// the exact generation it admitted, avoiding mutable process-global policy.
///
/// It also carries the run's public egress policy and ledger
/// ([`crate::egress::RunEgress`], #1490): every tool the policy admits runs
/// under it, and every client a family builds while materializing under this
/// policy is bound to it. Open unless the host lends one.
///
/// It also carries the run's approval policy ([`ApprovalPolicy`], ADR-0041
/// decision 5): a tool whose category the policy names for the run's tier is
/// sensitive like a tool the platform's sensitive tools name. None unless the
/// host lends one.
#[derive(Clone)]
pub struct ToolAdmissionPolicy {
    blocked_toolkits: HashSet<Box<str>>,
    /// Toolkit types under an active `toolkit_type` suspension, canonical
    /// keys. Main writes them into the run's frozen `toolkit_guardrails`
    /// (`suspended_toolkit_types`) at admission.
    suspended_toolkits: HashSet<Box<str>>,
    blocked_tools: HashMap<Box<str>, HashSet<Box<str>>>,
    sensitive_tools: HashMap<Box<str>, HashSet<Box<str>>>,
    sensitive_company_name: Box<str>,
    sensitive_message_template: Box<str>,
    egress: Arc<RunEgress>,
    approval: Arc<ApprovalPolicy>,
    run_caps: Arc<RunCapsMeter>,
    content: Arc<ContentControls>,
}

/// Model/UI-safe presentation for one sensitive concrete tool.
///
/// This value deliberately contains no raw arguments, secret, endpoint, or
/// approval identity. The call-bound interrupt owner adds those identities
/// only after the model emits one concrete function call.
#[derive(Clone, Eq, PartialEq)]
pub struct SensitiveToolPolicy {
    toolkit_type: Box<str>,
    toolkit_name: Box<str>,
    action_name: Box<str>,
    policy_message: Box<str>,
    rule: ApprovalRule,
}

impl SensitiveToolPolicy {
    #[must_use]
    pub const fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    #[must_use]
    pub const fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub const fn action_name(&self) -> &str {
        &self.action_name
    }

    #[must_use]
    pub const fn policy_message(&self) -> &str {
        &self.policy_message
    }

    /// The rule that makes the tool sensitive (ADR-0041 decision 5). The
    /// interrupt payload carries it as `approval_rule`.
    #[must_use]
    pub const fn rule(&self) -> ApprovalRule {
        self.rule
    }

    /// The cloud workspace's `run_command` when its command is destructive
    /// or escaping (ADR-0034 decision 9, ADR-0029 addendum item 3): asked
    /// as a server HITL interrupt before it runs.
    #[must_use]
    pub fn workspace_command() -> Self {
        Self {
            toolkit_type: "local".into(),
            toolkit_name: "local".into(),
            action_name: "run_command".into(),
            policy_message: "This command is destructive or reaches outside the workspace (a recursive delete, a history rewrite or push, a publish, privilege, or a write outside the workspace). Approve it to run it in the cloud workspace.".into(),
            rule: ApprovalRule::Builtin,
        }
    }

    /// The cloud executor's `open_pull_request` (ADR-0034 decision 10,
    /// amendment 2026-10-26): it pushes a new branch and opens a pull (merge)
    /// request on the user's forge, so it is asked as a server HITL interrupt
    /// on every call unless the policy lets them open without an approval.
    #[must_use]
    pub fn workspace_pull_request() -> Self {
        Self {
            toolkit_type: "forge".into(),
            toolkit_name: "workspace".into(),
            action_name: "open_pull_request".into(),
            policy_message: "The agent wants to push a new branch with this thread's commits to the folder's GitHub or GitLab repository and open a pull request (a merge request on GitLab). Approve it to push and open it.".into(),
            rule: ApprovalRule::Builtin,
        }
    }

    /// The cloud executor's `git_push` (same amendment): a new branch on the
    /// user's forge, asked like `open_pull_request`.
    #[must_use]
    pub fn workspace_git_push() -> Self {
        Self {
            toolkit_type: "forge".into(),
            toolkit_name: "workspace".into(),
            action_name: "git_push".into(),
            policy_message: "The agent wants to push this thread's commits to the folder's GitHub or GitLab repository as a new branch. Nothing existing is overwritten. Approve it to push.".into(),
            rule: ApprovalRule::Builtin,
        }
    }
}

impl ToolAdmissionPolicy {
    /// Normalize one trusted deployment configuration.
    ///
    /// Separator-only entries are ignored to match the current SDK. Duplicate
    /// canonical entries collapse without changing membership semantics.
    pub fn new(
        blocked_toolkits: &[String],
        blocked_tools: &BTreeMap<String, Vec<String>>,
    ) -> Result<Self, ToolAdmissionPolicyError> {
        Self::from_parts(
            blocked_toolkits,
            blocked_tools,
            &BTreeMap::new(),
            None,
            None,
        )
    }

    /// Parse the exact operator-owned `toolkit_security` dictionary.
    ///
    /// The worker service may atomically swap the resulting `Arc` when Main's
    /// runtime/admin configuration changes. This parser never reads ambient
    /// process variables, so one invocation cannot observe two generations.
    pub fn from_runtime_config(
        runtime: &Map<String, Value>,
    ) -> Result<Self, ToolAdmissionPolicyError> {
        let Some(security) = runtime.get("toolkit_security") else {
            return Self::new(&[], &BTreeMap::new());
        };
        let security = security.as_object().ok_or_else(invalid_configuration)?;
        if security.keys().any(|key| {
            !matches!(
                key.as_str(),
                "blocked_toolkits"
                    | "blocked_tools"
                    | "sensitive_tools"
                    | "sensitive_action_company_name"
                    | "sensitive_action_message_template"
                    | "suspended_toolkit_types"
            )
        }) {
            return Err(invalid_configuration());
        }
        let blocked_toolkits = parse_string_array(security.get("blocked_toolkits"))?;
        let suspended_toolkits = parse_string_array(security.get("suspended_toolkit_types"))?;
        let blocked_tools = parse_tool_map(security.get("blocked_tools"))?;
        let sensitive_tools = parse_tool_map(security.get("sensitive_tools"))?;
        let company_name = optional_policy_text(
            security.get("sensitive_action_company_name"),
            MAX_POLICY_COMPANY_BYTES,
        )?;
        let message_template = optional_policy_text(
            security.get("sensitive_action_message_template"),
            MAX_POLICY_TEMPLATE_BYTES,
        )?;
        let mut policy = Self::from_parts(
            &blocked_toolkits,
            &blocked_tools,
            &sensitive_tools,
            company_name,
            message_template,
        )?;
        policy.suspend_toolkits(&suspended_toolkits)?;
        Ok(policy)
    }

    /// Add toolkit types under an active suspension. The same canonical
    /// matching as `blocked_toolkits`, and the same identifier budget.
    ///
    /// # Errors
    ///
    /// Returns resource exhaustion for an over-limit list or identifier.
    pub fn suspend_toolkits(
        &mut self,
        toolkit_types: &[String],
    ) -> Result<(), ToolAdmissionPolicyError> {
        if toolkit_types.len() > MAX_POLICY_IDENTIFIERS {
            return Err(resource_exhausted());
        }
        for toolkit in toolkit_types {
            if let Some(key) = canonical_key(toolkit)? {
                self.suspended_toolkits.insert(key);
            }
        }
        Ok(())
    }

    fn from_parts(
        blocked_toolkits: &[String],
        blocked_tools: &BTreeMap<String, Vec<String>>,
        sensitive_tools: &BTreeMap<String, Vec<String>>,
        company_name: Option<&str>,
        message_template: Option<&str>,
    ) -> Result<Self, ToolAdmissionPolicyError> {
        validate_policy_size(blocked_toolkits, blocked_tools, sensitive_tools)?;

        let mut normalized_toolkits = HashSet::with_capacity(blocked_toolkits.len());
        for toolkit in blocked_toolkits {
            if let Some(key) = canonical_key(toolkit)? {
                normalized_toolkits.insert(key);
            }
        }

        let normalized_tools = normalize_tool_map(blocked_tools)?;
        let normalized_sensitive = normalize_tool_map(sensitive_tools)?;
        Ok(Self {
            blocked_toolkits: normalized_toolkits,
            suspended_toolkits: HashSet::new(),
            blocked_tools: normalized_tools,
            sensitive_tools: normalized_sensitive,
            sensitive_company_name: company_name
                .filter(|value| !value.trim().is_empty())
                .unwrap_or(DEFAULT_COMPANY_NAME)
                .into(),
            sensitive_message_template: message_template
                .filter(|value| !value.trim().is_empty())
                .unwrap_or(DEFAULT_MESSAGE_TEMPLATE)
                .into(),
            egress: RunEgress::open(),
            approval: ApprovalPolicy::none(),
            run_caps: RunCapsMeter::unlimited(),
            content: ContentControls::worker_default(),
        })
    }

    /// This generation bound to one run's public egress policy (#1490).
    #[must_use]
    pub fn with_egress(mut self, egress: Arc<RunEgress>) -> Self {
        self.egress = egress;
        self
    }

    /// The run's public egress policy and ledger.
    #[must_use]
    pub const fn egress(&self) -> &Arc<RunEgress> {
        &self.egress
    }

    /// This generation bound to one run's approval policy (ADR-0041
    /// decision 5). The policy only adds sensitive tools: a tool the
    /// platform's sensitive tools name stays sensitive.
    #[must_use]
    pub fn with_approval_policy(mut self, approval: Arc<ApprovalPolicy>) -> Self {
        self.approval = approval;
        self
    }

    /// The run's approval policy.
    #[must_use]
    pub const fn approval_policy(&self) -> &Arc<ApprovalPolicy> {
        &self.approval
    }

    /// This generation bound to one run's caps meter (ADR-0041 decision 3):
    /// every tool call it admits is counted against the run's caps. No cap
    /// unless the host lends a meter.
    #[must_use]
    pub fn with_run_caps(mut self, run_caps: Arc<RunCapsMeter>) -> Self {
        self.run_caps = run_caps;
        self
    }

    /// The run's caps meter.
    #[must_use]
    pub const fn run_caps(&self) -> &Arc<RunCapsMeter> {
        &self.run_caps
    }

    /// This generation bound to one run's content controls (ADR-0043
    /// decisions 3 and 4): the disclosure marker its outbound tools write,
    /// the preamble its agents get, and the run's untrusted reads. The
    /// worker defaults unless the host lends the run's.
    #[must_use]
    pub fn with_content_controls(mut self, content: Arc<ContentControls>) -> Self {
        self.content = content;
        self
    }

    /// The run's content controls.
    #[must_use]
    pub const fn content_controls(&self) -> &Arc<ContentControls> {
        &self.content
    }

    /// The "read untrusted, then act" rule for one tool the other rules do
    /// not name (ADR-0043 decision 4): when the rule is on for the run and
    /// the tool's category is `external_send`, `delete` or `payment`, the
    /// tool is a CONDITIONAL approval candidate. A host asks for an approval
    /// only for a call after the run read untrusted content
    /// ([`crate::untrusted::UntrustedReads::requires_approval`]).
    #[must_use]
    pub fn untrusted_act_tool_in(
        &self,
        toolkit_type: &str,
        toolkit_name: &str,
        settings: Option<&Map<String, Value>>,
        tool_name: &str,
    ) -> Option<SensitiveToolPolicy> {
        if !self.content.untrusted().act_rule() {
            return None;
        }
        let category = tool_category(toolkit_type, settings, tool_name);
        crate::untrusted::is_act_category(category).then(|| {
            self.sensitive_presentation(
                toolkit_type,
                toolkit_name,
                tool_name,
                ApprovalRule::UntrustedReadThenAct { category },
            )
        })
    }

    #[must_use]
    pub fn toolkit_decision(&self, toolkit_type: &str) -> ToolAdmissionDecision {
        let Some(key) = canonical_runtime_key(toolkit_type) else {
            return ToolAdmissionDecision::Allowed;
        };
        if self.suspended_toolkits.contains(key.as_str()) {
            ToolAdmissionDecision::SuspendedToolkit
        } else if self.blocked_toolkits.contains(key.as_str()) {
            ToolAdmissionDecision::BlockedToolkit
        } else {
            ToolAdmissionDecision::Allowed
        }
    }

    #[must_use]
    pub fn tool_decision(&self, toolkit_type: &str, tool_name: &str) -> ToolAdmissionDecision {
        let toolkit = self.toolkit_decision(toolkit_type);
        if toolkit != ToolAdmissionDecision::Allowed {
            return toolkit;
        }
        let Some(toolkit_key) = canonical_runtime_key(toolkit_type) else {
            return ToolAdmissionDecision::Allowed;
        };
        let Some(blocked) = self.blocked_tools.get(toolkit_key.as_str()) else {
            return ToolAdmissionDecision::Allowed;
        };
        if any_alias_matches(tool_name, blocked) {
            ToolAdmissionDecision::BlockedTool
        } else {
            ToolAdmissionDecision::Allowed
        }
    }

    /// Resolve a configured sensitive-tool rule against toolkit type, toolkit
    /// display/name identity, wildcard, and the SDK's prefixed tool aliases,
    /// then the approval policy against the tool's declared category.
    #[must_use]
    pub fn sensitive_tool(
        &self,
        toolkit_type: &str,
        toolkit_name: &str,
        tool_name: &str,
    ) -> Option<SensitiveToolPolicy> {
        self.sensitive_tool_in(toolkit_type, toolkit_name, None, tool_name)
    }

    /// [`Self::sensitive_tool`] for a toolkit whose settings are at hand: an
    /// approved MCP server's pins give its tools their category.
    ///
    /// The platform rule wins when both rules name the tool. Neither rule
    /// reads anything an agent author sets, except the pins' category for a
    /// tool the contract does not declare; the administrator who approved
    /// the server set those.
    #[must_use]
    pub fn sensitive_tool_in(
        &self,
        toolkit_type: &str,
        toolkit_name: &str,
        settings: Option<&Map<String, Value>>,
        tool_name: &str,
    ) -> Option<SensitiveToolPolicy> {
        let platform = [toolkit_type, toolkit_name]
            .into_iter()
            .filter_map(canonical_runtime_key)
            .chain(std::iter::once("*".to_owned()))
            .any(|identifier| {
                self.sensitive_tools
                    .get(identifier.as_str())
                    .is_some_and(|tools| any_alias_matches(tool_name, tools))
            });
        let rule = if platform {
            ApprovalRule::Platform
        } else {
            self.approval
                .rule_for(tool_category(toolkit_type, settings, tool_name))?
        };
        Some(self.sensitive_presentation(toolkit_type, toolkit_name, tool_name, rule))
    }

    fn sensitive_presentation(
        &self,
        toolkit_type: &str,
        toolkit_name: &str,
        tool_name: &str,
        rule: ApprovalRule,
    ) -> SensitiveToolPolicy {
        let toolkit_label = if toolkit_name.trim().is_empty() {
            toolkit_type.trim()
        } else {
            toolkit_name.trim()
        };
        let action_name = format!("{toolkit_label}.{tool_name}");
        let policy_message = render_policy_message(
            &self.sensitive_message_template,
            &self.sensitive_company_name,
            tool_name,
            toolkit_label,
            &action_name,
        );
        SensitiveToolPolicy {
            toolkit_type: toolkit_type.trim().into(),
            toolkit_name: toolkit_name.trim().into(),
            action_name: action_name.into(),
            policy_message: policy_message.into(),
            rule,
        }
    }
}

fn validate_policy_size(
    blocked_toolkits: &[String],
    blocked_tools: &BTreeMap<String, Vec<String>>,
    sensitive_tools: &BTreeMap<String, Vec<String>>,
) -> Result<(), ToolAdmissionPolicyError> {
    let initial = blocked_toolkits
        .len()
        .checked_add(blocked_tools.len())
        .and_then(|value| value.checked_add(sensitive_tools.len()));
    let total = blocked_tools
        .values()
        .chain(sensitive_tools.values())
        .fold(initial, |total, tools| {
            total.and_then(|value| value.checked_add(tools.len()))
        });
    if total.is_none_or(|total| total > MAX_POLICY_IDENTIFIERS) {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn normalize_tool_map(
    source: &BTreeMap<String, Vec<String>>,
) -> Result<HashMap<Box<str>, HashSet<Box<str>>>, ToolAdmissionPolicyError> {
    let mut normalized = HashMap::with_capacity(source.len());
    for (toolkit, tools) in source {
        let Some(toolkit_key) = canonical_toolkit_policy_key(toolkit)? else {
            continue;
        };
        let entry = normalized.entry(toolkit_key).or_insert_with(HashSet::new);
        for tool in tools {
            if let Some(tool_key) = canonical_key(tool)? {
                entry.insert(tool_key);
            }
        }
    }
    Ok(normalized)
}

fn parse_string_array(value: Option<&Value>) -> Result<Vec<String>, ToolAdmissionPolicyError> {
    let Some(value) = value else {
        return Ok(Vec::new());
    };
    let values = value.as_array().ok_or_else(invalid_configuration)?;
    values
        .iter()
        .map(|value| {
            value
                .as_str()
                .map(str::to_owned)
                .ok_or_else(invalid_configuration)
        })
        .collect()
}

fn parse_tool_map(
    value: Option<&Value>,
) -> Result<BTreeMap<String, Vec<String>>, ToolAdmissionPolicyError> {
    let Some(value) = value else {
        return Ok(BTreeMap::new());
    };
    let object = value.as_object().ok_or_else(invalid_configuration)?;
    object
        .iter()
        .map(|(toolkit, tools)| Ok((toolkit.clone(), parse_string_array(Some(tools))?)))
        .collect()
}

fn optional_policy_text(
    value: Option<&Value>,
    maximum: usize,
) -> Result<Option<&str>, ToolAdmissionPolicyError> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.len() <= maximum && !value.contains('\0') => {
            Ok(Some(value))
        }
        Some(Value::String(_)) => Err(resource_exhausted()),
        Some(_) => Err(invalid_configuration()),
    }
}

fn render_policy_message(
    template: &str,
    company_name: &str,
    tool_name: &str,
    toolkit_label: &str,
    action_name: &str,
) -> String {
    let mut rendered = template.to_owned();
    for (placeholder, value) in [
        ("{company_name}", company_name),
        ("{tool_name}", tool_name),
        ("{toolkit_name}", toolkit_label),
        ("{toolkit_type}", toolkit_label),
        ("{toolkit_label}", toolkit_label),
        ("{action_name}", action_name),
    ] {
        rendered = rendered.replace(placeholder, value);
    }
    if rendered.is_empty()
        || rendered.len() > MAX_POLICY_MESSAGE_BYTES
        || rendered.contains(['{', '}'])
        || rendered.contains('\0')
    {
        format!(
            "{company_name} requires approval before running the sensitive action '{action_name}'."
        )
    } else {
        rendered
    }
}

fn canonical_key(value: &str) -> Result<Option<Box<str>>, ToolAdmissionPolicyError> {
    if value.len() > MAX_POLICY_IDENTIFIER_BYTES {
        return Err(resource_exhausted());
    }
    Ok(canonical_runtime_key(value).map(String::into_boxed_str))
}

fn canonical_toolkit_policy_key(value: &str) -> Result<Option<Box<str>>, ToolAdmissionPolicyError> {
    if value.len() > MAX_POLICY_IDENTIFIER_BYTES {
        return Err(resource_exhausted());
    }
    if value.trim() == "*" {
        return Ok(Some(Box::from("*")));
    }
    Ok(canonical_runtime_key(value).map(String::into_boxed_str))
}

fn canonical_runtime_key(value: &str) -> Option<String> {
    let key = value
        .trim()
        .chars()
        .flat_map(char::to_lowercase)
        .filter(char::is_ascii_alphanumeric)
        .collect::<String>();
    (!key.is_empty()).then_some(key)
}

fn any_alias_matches(tool_name: &str, blocked: &HashSet<Box<str>>) -> bool {
    let mut current = tool_name.trim().to_lowercase();
    while !current.is_empty() {
        if canonical_runtime_key(&current).is_some_and(|key| blocked.contains(key.as_str())) {
            return true;
        }
        let mut reduced = current.as_str();
        if let Some((_, suffix)) = reduced.split_once("___") {
            reduced = suffix.trim();
        }
        if let Some((_, suffix)) = reduced.split_once(':') {
            reduced = suffix.trim();
        }
        if reduced.is_empty() || reduced == current {
            break;
        }
        current = reduced.to_owned();
    }
    false
}

const fn resource_exhausted() -> ToolAdmissionPolicyError {
    ToolAdmissionPolicyError {
        code: ToolAdmissionPolicyErrorCode::ResourceExhausted,
    }
}

const fn invalid_configuration() -> ToolAdmissionPolicyError {
    ToolAdmissionPolicyError {
        code: ToolAdmissionPolicyErrorCode::InvalidConfiguration,
    }
}

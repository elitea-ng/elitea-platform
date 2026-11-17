//! Delegation narrowing (ADR-0044 decision 3, #1501).
//!
//! When an agent calls a saved agent as a tool, an agent calls a saved
//! pipeline, a pipeline node runs a saved agent or pipeline, or an agent
//! starts a sub-agent, the callee runs with the INTERSECTION of its own
//! grants and its caller's effective grants. Four grant kinds are covered:
//!
//! - toolkits and their tools (a toolkit is one toolkit row, by id; a
//!   built-in toolkit without a row is named by its type);
//! - credential references (ADR-0039): a callee toolkit that names a
//!   reference may use it only when the caller holds it;
//! - data classes, as declared in the agent registry (declared metadata, not
//!   classifier output);
//! - egress hosts: the hosts a callee toolkit is configured to reach must be
//!   on the run's egress allowlist.
//!
//! A callee that needs a grant its caller does not hold is refused with
//! [`GRANT_MISSING`], naming the grant. A callee whose tool selection is
//! wider than its caller's (it selects every tool, the caller a few) is
//! narrowed, not refused: it needs no specific tool the caller lacks.
//!
//! Delegation depth counts every kind together (agent tool, pipeline child,
//! pipeline node, sub-agent). The root is depth 0; a delegation past the
//! administrator's maximum (default [`DEFAULT_MAX_DEPTH`]) is refused with
//! [`DEPTH_EXCEEDED`]. The per-kind limits (agent tiers, the pipeline
//! participant limit) still apply.
//!
//! Each delegation carries [`DelegationEvidence`]: the depth and the SHA-256
//! of the callee's effective grant set, in a canonical sorted form. The run
//! step of the delegation records both.
//!
//! An ad hoc conversation root has no saved definition, so it imposes no
//! toolkit grant of its own: the person chose its participants. Caller
//! narrowing starts at the first saved agent or pipeline.
//!
//! # The run principal (ADR-0044 Amendment 1, #2157)
//!
//! Every callee's effective set is also intersected with the authority of the
//! run principal `P`: the acting user, or the mandate and project of an
//! unattended run. Main computes it for the reachable closure of the root and
//! sends it as [`PrincipalBound`] on the policy. The effective set is
//!
//! ```text
//! E(X) = own(X) ∩ (E(Y) ∪ G(Y→X)) ∩ P
//! ```
//!
//! and this module implements the `∩ P` term: the root is `own(root) ∩ P`
//! (see [`DelegationScope::root`] and
//! [`AdmittedToolSnapshot::retain_principal`]), and every delegation (agent
//! tool, pipeline child, pipeline node, sub-agent, swarm member) is refused
//! with [`GRANT_MISSING`] and reason `principal` when its callee needs
//! something outside `P`. `P` applies in both [`NarrowingMode`]s: `report`
//! never relaxes it. `G(Y→X)`, the approved delegation grant of the edge,
//! unions with `E(Y)` in the scope's `caller_ceiling`; until it lands
//! `G` is empty, and `P` is applied after that union, so a grant can never
//! reach past the principal.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use ring::digest;
use serde_json::{Map, Value, json};

use crate::egress::PublicPolicy;
use crate::toolkits::{AdmittedToolSnapshot, FrozenToolKind};

mod grants;

pub use grants::{
    DelegationGrant, EdgeKey, EdgeKind, GrantEvidence, Grants, MAX_GRANT_EDGES, Narrowed,
    narrow_with_grant,
};

/// The depth a run may delegate to when the administrator set none.
pub const DEFAULT_MAX_DEPTH: u32 = 4;
/// The smallest maximum an administrator may set.
pub const MIN_MAX_DEPTH: u32 = 1;
/// The largest maximum an administrator may set.
pub const MAX_MAX_DEPTH: u32 = 16;
/// The most data classes one agent declares (the registry's bound).
pub const MAX_DATA_CLASSES: usize = 32;
/// The longest data class (the registry's grammar).
pub const MAX_DATA_CLASS_BYTES: usize = 64;

/// The refusal code of a callee that needs a grant its caller lacks.
pub const GRANT_MISSING: &str = "DELEGATION_GRANT_MISSING";
/// The refusal code of a delegation past the maximum depth.
pub const DEPTH_EXCEEDED: &str = "DELEGATION_DEPTH_EXCEEDED";

/// The schema tag of a grant set's canonical form. Main writes the same form
/// into a delegation grant (`delegationgrant.Schema`), and both run
/// `libs/proto/contracts/delegation-grant-digest-vectors-v1.json`.
pub const GRANT_SET_SCHEMA: &str = "elitea.delegation.grants.v1";

/// The most toolkit references one grant set reads.
const MAX_TOOLKITS: usize = 1_024;
/// The most tool names one toolkit selection reads.
const MAX_TOOL_NAMES: usize = 1_024;
/// The most credential references a principal bound names.
pub const MAX_PRINCIPAL_CREDENTIALS: usize = 256;
/// The most toolkits a principal bound names.
pub const MAX_PRINCIPAL_TOOLKITS: usize = 512;
/// The longest credential reference id a principal bound names.
pub const MAX_PRINCIPAL_REFERENCE_BYTES: usize = 128;
/// The longest tool name a principal bound names.
pub const MAX_PRINCIPAL_TOOL_BYTES: usize = 256;
/// The deepest a settings walk for hosts goes.
const MAX_HOST_WALK_DEPTH: usize = 16;
/// The most declared hosts one callee is checked for.
const MAX_DECLARED_HOSTS: usize = 256;

/// Whether grant narrowing refuses or only reports.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum NarrowingMode {
    /// A missing grant refuses the delegation (the default).
    #[default]
    Enforce,
    /// A missing grant is recorded on the delegation step, and the callee
    /// runs with its own grants. For an upgrade window only. The depth cap
    /// is enforced in both modes.
    Report,
}

impl NarrowingMode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Enforce => "enforce",
            Self::Report => "report",
        }
    }
}

/// Why a delegation policy was refused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DelegationPolicyError {
    /// The maximum depth is outside [`MIN_MAX_DEPTH`]..=[`MAX_MAX_DEPTH`].
    DepthOutOfRange,
    /// A data class is not a short identifier.
    MalformedDataClass,
    /// More than [`MAX_DATA_CLASSES`] data classes.
    TooManyDataClasses,
    /// A principal bound with more entries than its limits, or a name that
    /// is too long.
    PrincipalBoundTooLarge,
    /// A delegation grant whose set does not hash to its digest, or with no
    /// id.
    MalformedGrant,
    /// More than [`MAX_GRANT_EDGES`] delegation grants.
    TooManyGrants,
}

impl fmt::Display for DelegationPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::DepthOutOfRange => "the maximum delegation depth is out of range",
            Self::MalformedDataClass => "a data class is not a short identifier",
            Self::TooManyDataClasses => "the run declares more data classes than the limit",
            Self::PrincipalBoundTooLarge => "the run principal's bound is larger than the limit",
            Self::MalformedGrant => "a delegation grant does not match its digest",
            Self::TooManyGrants => "the run carries more delegation grants than the limit",
        })
    }
}

impl std::error::Error for DelegationPolicyError {}

/// The run's delegation policy: the administrator's maximum depth and mode,
/// and the root agent's declared data classes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DelegationPolicy {
    max_depth: u32,
    mode: NarrowingMode,
    root_data_classes: BTreeSet<String>,
    principal: Option<PrincipalBound>,
    /// The approved delegation grants of the run, by edge (ADR-0044
    /// Amendment 1, #2166). Empty unless Main's grant switch is on.
    grants: Grants,
    /// The saved agent or pipeline the run starts with; 0 for none.
    root_application_id: u64,
}

impl Default for DelegationPolicy {
    fn default() -> Self {
        Self {
            max_depth: DEFAULT_MAX_DEPTH,
            mode: NarrowingMode::Enforce,
            root_data_classes: BTreeSet::new(),
            principal: None,
            grants: Grants::new(),
            root_application_id: 0,
        }
    }
}

impl DelegationPolicy {
    /// A validated policy.
    ///
    /// # Errors
    ///
    /// [`DelegationPolicyError`] for a depth out of range or a malformed or
    /// excess data class list.
    pub fn new(
        max_depth: u32,
        mode: NarrowingMode,
        root_data_classes: impl IntoIterator<Item = String>,
    ) -> Result<Self, DelegationPolicyError> {
        if !(MIN_MAX_DEPTH..=MAX_MAX_DEPTH).contains(&max_depth) {
            return Err(DelegationPolicyError::DepthOutOfRange);
        }
        Ok(Self {
            max_depth,
            mode,
            root_data_classes: data_classes(root_data_classes)?,
            principal: None,
            grants: Grants::new(),
            root_application_id: 0,
        })
    }

    /// This policy with the run's approved delegation grants, which also turns
    /// grant enforcement on: an edge that has one is bounded by `E(Y) ∪ G`.
    ///
    /// # Errors
    ///
    /// [`DelegationPolicyError::TooManyGrants`] for more than
    /// [`MAX_GRANT_EDGES`].
    pub fn with_grants(
        mut self,
        root_application_id: u64,
        grants: impl IntoIterator<Item = (EdgeKey, DelegationGrant)>,
    ) -> Result<Self, DelegationPolicyError> {
        self.root_application_id = root_application_id;
        for (edge, grant) in grants {
            self.grants.insert(edge, grant);
            if self.grants.len() > MAX_GRANT_EDGES {
                return Err(DelegationPolicyError::TooManyGrants);
            }
        }
        Ok(self)
    }

    /// The approved grant of an edge, when the run carries grants.
    #[must_use]
    pub fn grant(&self, edge: &EdgeKey) -> Option<&DelegationGrant> {
        self.grants.get(edge)
    }

    /// The approved grant between a caller and a callee, whatever the kind of
    /// edge. The worker reaches a callee through an application tool: an agent
    /// calls it as a tool (`agent_tool`) and a pipeline runs it as a child
    /// (`pipeline_child`), which is what attaching creates, so those two are
    /// read first. A `sub_agent` or `pipeline_node` grant proposed through the
    /// API for the same pair is read only when neither exists.
    #[must_use]
    pub fn grant_between(&self, caller: u64, callee: u64) -> Option<&DelegationGrant> {
        [
            EdgeKind::AgentTool,
            EdgeKind::PipelineChild,
            EdgeKind::SubAgent,
            EdgeKind::PipelineNode,
        ]
        .into_iter()
        .find_map(|kind| {
            self.grants.get(&EdgeKey {
                caller,
                callee,
                kind,
            })
        })
    }

    /// Whether the run carries grants (Main's switch is on and some edge of
    /// the root's closure has one).
    #[must_use]
    pub fn grants_enforced(&self) -> bool {
        !self.grants.is_empty()
    }

    /// The saved agent or pipeline the run starts with, 0 for none.
    #[must_use]
    pub const fn root_application_id(&self) -> u64 {
        self.root_application_id
    }

    /// This policy bounded by the run principal's authority `P`.
    #[must_use]
    pub fn with_principal(mut self, principal: PrincipalBound) -> Self {
        self.principal = Some(principal);
        self
    }

    /// The run principal's bound, when Main sent one.
    #[must_use]
    pub const fn principal(&self) -> Option<&PrincipalBound> {
        self.principal.as_ref()
    }

    #[must_use]
    pub const fn max_depth(&self) -> u32 {
        self.max_depth
    }

    #[must_use]
    pub const fn mode(&self) -> NarrowingMode {
        self.mode
    }

    #[must_use]
    pub const fn root_data_classes(&self) -> &BTreeSet<String> {
        &self.root_data_classes
    }
}

/// Whether `class` is a data class in the registry's grammar
/// (`^[a-z0-9][a-z0-9_.-]{0,63}$`).
#[must_use]
pub fn valid_data_class(class: &str) -> bool {
    let bytes = class.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= MAX_DATA_CLASS_BYTES
        && (bytes[0].is_ascii_lowercase() || bytes[0].is_ascii_digit())
        && bytes.iter().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'_' | b'.' | b'-')
        })
}

/// A validated, de-duplicated data class set.
///
/// # Errors
///
/// [`DelegationPolicyError`] for a malformed class or more than
/// [`MAX_DATA_CLASSES`].
pub fn data_classes(
    classes: impl IntoIterator<Item = String>,
) -> Result<BTreeSet<String>, DelegationPolicyError> {
    let mut set = BTreeSet::new();
    for class in classes {
        if !valid_data_class(&class) {
            return Err(DelegationPolicyError::MalformedDataClass);
        }
        set.insert(class);
        if set.len() > MAX_DATA_CLASSES {
            return Err(DelegationPolicyError::TooManyDataClasses);
        }
    }
    Ok(set)
}

/// The kind of grant a refusal names.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum GrantKind {
    Toolkit,
    Tool,
    Credential,
    DataClass,
    EgressHost,
}

impl GrantKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Toolkit => "toolkit",
            Self::Tool => "tool",
            Self::Credential => "credential",
            Self::DataClass => "data_class",
            Self::EgressHost => "egress_host",
        }
    }
}

/// A refused delegation. The model reads it as a tool result, and the
/// delegation step records it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum DelegationRefusal {
    /// The callee needs a grant its caller does not hold.
    GrantMissing {
        kind: GrantKind,
        /// The grant: the toolkit name, the tool name, the credential
        /// reference id, the data class or the host.
        name: String,
        /// For a tool or credential, the toolkit it belongs to.
        toolkit: Option<String>,
    },
    /// The callee needs something the run principal (the acting user, or the
    /// mandate and project) may not use (ADR-0044 Amendment 1). Refused in
    /// both modes. The code is [`GRANT_MISSING`]; the structured form carries
    /// `"reason": "principal"`.
    PrincipalBound {
        kind: GrantKind,
        name: String,
        toolkit: Option<String>,
    },
    /// The delegation would pass the maximum depth.
    DepthExceeded { depth: u32, max_depth: u32 },
}

impl DelegationRefusal {
    /// [`GRANT_MISSING`] or [`DEPTH_EXCEEDED`].
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::GrantMissing { .. } | Self::PrincipalBound { .. } => GRANT_MISSING,
            Self::DepthExceeded { .. } => DEPTH_EXCEEDED,
        }
    }

    /// The sentence the model and the run read.
    #[must_use]
    pub fn message(&self) -> String {
        match self {
            Self::GrantMissing {
                kind,
                name,
                toolkit,
            } => {
                let grant = grant_text(*kind, name, toolkit.as_deref());
                format!(
                    "{GRANT_MISSING}: the delegated agent needs {grant}, and its caller does not hold it. A delegated agent runs with the intersection of its own grants and its caller's. Grant it to the caller, or remove it from the delegated agent."
                )
            }
            Self::PrincipalBound {
                kind,
                name,
                toolkit,
            } => {
                let grant = grant_text(*kind, name, toolkit.as_deref());
                format!(
                    "{GRANT_MISSING} (principal): the delegated agent needs {grant}, and the person or mandate this run acts for may not use it. A delegated agent never exceeds the authority of the run's principal, whatever its caller holds. Ask a project administrator for access, or remove it from the delegated agent."
                )
            }
            Self::DepthExceeded { depth, max_depth } => format!(
                "{DEPTH_EXCEEDED}: this delegation would run at depth {depth}, deeper than the maximum of {max_depth} the administrator set. Do the work at this level, or ask an administrator to raise the delegation depth."
            ),
        }
    }

    /// The structured form a step record and a tool result carry.
    #[must_use]
    pub fn to_value(&self) -> Value {
        match self {
            Self::GrantMissing {
                kind,
                name,
                toolkit,
            } => {
                let mut value = json!({"code": GRANT_MISSING, "kind": kind.as_str(), "name": name});
                if let Some(toolkit) = toolkit {
                    value["toolkit"] = json!(toolkit);
                }
                value
            }
            Self::PrincipalBound {
                kind,
                name,
                toolkit,
            } => {
                let mut value = json!({
                    "code": GRANT_MISSING,
                    "reason": "principal",
                    "kind": kind.as_str(),
                    "name": name,
                });
                if let Some(toolkit) = toolkit {
                    value["toolkit"] = json!(toolkit);
                }
                value
            }
            Self::DepthExceeded { depth, max_depth } => {
                json!({"code": DEPTH_EXCEEDED, "depth": depth, "max_depth": max_depth})
            }
        }
    }

    /// The tool error a refused call fails with, for a host that reports a
    /// refusal as an error rather than a result.
    #[must_use]
    pub fn tool_error(&self) -> AdkError {
        AdkError::new(
            ErrorComponent::Guardrail,
            ErrorCategory::Forbidden,
            self.code(),
            self.message(),
        )
        .with_retry(RetryHint {
            should_retry: false,
            retry_after_ms: None,
            max_attempts: Some(1),
        })
    }
}

/// The phrase a refusal names the grant by.
fn grant_text(kind: GrantKind, name: &str, toolkit: Option<&str>) -> String {
    match (kind, toolkit) {
        (GrantKind::Toolkit, _) => format!("the toolkit '{name}'"),
        (GrantKind::Tool, Some(toolkit)) => {
            format!("the tool '{name}' of the toolkit '{toolkit}'")
        }
        (GrantKind::Tool, None) => format!("the tool '{name}'"),
        (GrantKind::Credential, Some(toolkit)) => {
            format!("the credential reference '{name}' of the toolkit '{toolkit}'")
        }
        (GrantKind::Credential, None) => format!("the credential reference '{name}'"),
        (GrantKind::DataClass, _) => format!("the data class '{name}'"),
        (GrantKind::EgressHost, _) => {
            format!("egress to the host '{name}' (it is not on this run's allowlist)")
        }
    }
}

impl fmt::Display for DelegationRefusal {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message())
    }
}

impl std::error::Error for DelegationRefusal {}

/// Which tools of a toolkit a grant covers.
#[derive(Clone, Debug, Eq, PartialEq)]
enum ToolSelection {
    /// Every tool but these.
    All { excluded: BTreeSet<String> },
    /// Only these tools.
    Only(BTreeSet<String>),
}

impl ToolSelection {
    fn allows(&self, tool: &str) -> bool {
        match self {
            Self::All { excluded } => !excluded.contains(tool),
            Self::Only(tools) => tools.contains(tool),
        }
    }

    /// The intersection of two selections.
    fn intersect(&self, other: &Self) -> Self {
        match (self, other) {
            (Self::All { excluded: a }, Self::All { excluded: b }) => Self::All {
                excluded: a.union(b).cloned().collect(),
            },
            (Self::All { excluded }, Self::Only(tools))
            | (Self::Only(tools), Self::All { excluded }) => {
                Self::Only(tools.difference(excluded).cloned().collect())
            }
            (Self::Only(a), Self::Only(b)) => Self::Only(a.intersection(b).cloned().collect()),
        }
    }

    fn canonical(&self) -> Value {
        match self {
            Self::All { excluded } => json!({"all_except": excluded}),
            Self::Only(tools) => json!({"only": tools}),
        }
    }
}

/// One toolkit grant.
#[derive(Clone, Debug, Eq, PartialEq)]
struct ToolkitGrant {
    /// The toolkit's name, for messages.
    label: String,
    selection: ToolSelection,
    credential: Option<String>,
}

/// A caller's or callee's grants.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct GrantSet {
    /// `None`: no toolkit constraint (an ad hoc conversation root).
    toolkits: Option<BTreeMap<String, ToolkitGrant>>,
    /// `None`: no data class constraint.
    data_classes: Option<BTreeSet<String>>,
    /// The hosts the toolkits are configured to reach, as checked.
    egress_hosts: BTreeSet<String>,
}

impl GrantSet {
    /// No constraint at all: the root of an ad hoc conversation.
    #[must_use]
    pub fn unconstrained() -> Self {
        Self::default()
    }

    /// The grants of a frozen tool list (a version's `tools`). Nested
    /// application references are not grants: delegating to them is what
    /// this module governs.
    #[must_use]
    pub fn from_tools(tools: &[Value]) -> Self {
        let mut toolkits = BTreeMap::new();
        for tool in tools.iter().take(MAX_TOOLKITS) {
            let Some(object) = tool.as_object() else {
                continue;
            };
            let tool_type = object.get("type").and_then(Value::as_str).unwrap_or("");
            if tool_type.is_empty() || tool_type == "application" {
                continue;
            }
            let tool_id = object.get("id").and_then(Value::as_u64);
            let label = object
                .get("toolkit_name")
                .and_then(Value::as_str)
                .unwrap_or(tool_type);
            let settings = object.get("settings").and_then(Value::as_object);
            let (key, grant) = toolkit_grant(tool_id, tool_type, label, settings);
            toolkits.insert(key, grant);
        }
        Self {
            toolkits: Some(toolkits),
            data_classes: None,
            egress_hosts: BTreeSet::new(),
        }
    }

    /// What a callee holds, as Main records it in a delegation grant: the
    /// toolkits of [`Self::from_tools`], the hosts its toolkits are configured
    /// to reach ([`declared_hosts`], host only) and the data classes it
    /// declares (none imposes nothing). `delegationgrant.FromTools` in Main
    /// builds the same set; both run the shared `tools_cases` vectors.
    #[must_use]
    pub fn from_callee(tools: &[Value], callee_data_classes: &BTreeSet<String>) -> Self {
        let mut set = Self::from_tools(tools).with_data_classes(callee_data_classes);
        set.egress_hosts = declared_hosts(tools)
            .into_iter()
            .map(|(host, _)| host)
            .collect();
        set
    }

    /// The grants of an admitted snapshot (the root's own).
    #[must_use]
    pub fn from_snapshot(snapshot: &AdmittedToolSnapshot<'_>) -> Self {
        let mut toolkits = BTreeMap::new();
        for reference in snapshot
            .iter()
            .filter(|reference| reference.kind() != FrozenToolKind::Application)
        {
            let (key, grant) = toolkit_grant(
                reference.tool_id(),
                reference.tool_type(),
                reference.toolkit_name(),
                reference.settings(),
            );
            toolkits.insert(key, grant);
        }
        Self {
            toolkits: Some(toolkits),
            data_classes: None,
            egress_hosts: BTreeSet::new(),
        }
    }

    /// The grants of a stored delegation grant: the canonical form
    /// [`Self::canonical`] writes (the `grant_set` Main stores and sends).
    /// `from_canonical(&set.canonical())` is `set`, and the digest of the
    /// result is the digest Main pinned.
    ///
    /// # Errors
    ///
    /// [`GrantSetError`] for a document that is not the canonical form, or
    /// that is past the bounds a set is built within.
    pub fn from_canonical(value: &Value) -> Result<Self, GrantSetError> {
        let object = value.as_object().ok_or(GrantSetError)?;
        if object.get("schema").and_then(Value::as_str) != Some(GRANT_SET_SCHEMA) {
            return Err(GrantSetError);
        }
        let entries = object
            .get("toolkits")
            .and_then(Value::as_array)
            .ok_or(GrantSetError)?;
        if entries.len() > MAX_TOOLKITS {
            return Err(GrantSetError);
        }
        let mut toolkits = BTreeMap::new();
        for entry in entries {
            let (key, grant) = canonical_toolkit(entry)?;
            toolkits.insert(key, grant);
        }
        let classes = match object.get("data_classes") {
            Some(Value::Null) | None => None,
            Some(Value::Array(listed)) => {
                let listed = string_list(listed, MAX_DATA_CLASSES)?;
                let set = data_classes(listed).map_err(|_| GrantSetError)?;
                (!set.is_empty()).then_some(set)
            }
            Some(_) => return Err(GrantSetError),
        };
        let hosts = object
            .get("egress_hosts")
            .and_then(Value::as_array)
            .ok_or(GrantSetError)?;
        Ok(Self {
            toolkits: Some(toolkits),
            data_classes: classes,
            egress_hosts: string_list(hosts, MAX_DECLARED_HOSTS)?
                .into_iter()
                .collect(),
        })
    }

    /// These grants limited to `classes`; an empty set imposes nothing.
    #[must_use]
    pub fn with_data_classes(mut self, classes: &BTreeSet<String>) -> Self {
        self.data_classes = (!classes.is_empty()).then(|| classes.clone());
        self
    }

    /// These grants limited to what the run principal may use: a toolkit the
    /// principal may not use, or whose credential reference it may not use,
    /// is removed; a tool selection is cut to the principal's; the data
    /// classes are cut to the project's (own `∩ P`).
    #[must_use]
    pub fn bounded_by(mut self, principal: &PrincipalBound) -> Self {
        if let Some(toolkits) = self.toolkits.as_mut() {
            toolkits.retain(|key, grant| {
                if !principal.admits_toolkit(toolkit_id(key)) {
                    return false;
                }
                if let Some(credential) = &grant.credential
                    && !principal.admits_credential(credential)
                {
                    return false;
                }
                if let Some(id) = toolkit_id(key)
                    && let Some(allowed) = principal.toolkits.get(&id)
                {
                    grant.selection = allowed.intersect(&grant.selection);
                }
                !matches!(&grant.selection, ToolSelection::Only(tools) if tools.is_empty())
            });
        }
        if !principal.data_classes.is_empty() {
            self.data_classes = Some(match self.data_classes.take() {
                Some(held) => held
                    .intersection(&principal.data_classes)
                    .cloned()
                    .collect(),
                None => principal.data_classes.clone(),
            });
        }
        self
    }

    /// Whether this set constrains toolkits (it is not an ad hoc root).
    #[must_use]
    pub const fn constrains_toolkits(&self) -> bool {
        self.toolkits.is_some()
    }

    /// The canonical, sorted form the digest is computed over.
    #[must_use]
    pub fn canonical(&self) -> Value {
        let toolkits = self.toolkits.as_ref().map(|toolkits| {
            toolkits
                .iter()
                .map(|(key, grant)| {
                    json!({
                        "key": key,
                        "tools": grant.selection.canonical(),
                        "credential": grant.credential,
                    })
                })
                .collect::<Vec<_>>()
        });
        json!({
            "schema": GRANT_SET_SCHEMA,
            "toolkits": toolkits,
            "data_classes": self.data_classes,
            "egress_hosts": self.egress_hosts,
        })
    }

    /// The lowercase hex SHA-256 of [`Self::canonical`].
    #[must_use]
    pub fn digest(&self) -> String {
        hex_sha256(&self.canonical())
    }

    fn credentials(&self) -> Option<BTreeSet<&str>> {
        self.toolkits.as_ref().map(|toolkits| {
            toolkits
                .values()
                .filter_map(|grant| grant.credential.as_deref())
                .collect()
        })
    }
}

/// The id of a keyed toolkit (`toolkit:<id>`); a built-in (`builtin:<type>`)
/// has none.
fn toolkit_id(key: &str) -> Option<u64> {
    key.strip_prefix("toolkit:")?.parse().ok()
}

/// One toolkit the run principal may use.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PrincipalToolkit {
    pub id: u64,
    /// `None`: every tool. `Some`: only these.
    pub tools: Option<BTreeSet<String>>,
}

/// What the run principal may use (ADR-0044 Amendment 1, rule 1): the acting
/// user, or the mandate and project of an unattended run. Main computes it
/// for the reachable closure of the root. Whatever it does not list is out of
/// bounds, so an empty list means nothing; "no bound" is the absence of the
/// whole value (see [`DelegationPolicy::principal`]).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PrincipalBound {
    toolkits: BTreeMap<u64, ToolSelection>,
    /// `None`: no credential bound (an ungoverned run).
    credentials: Option<BTreeSet<String>>,
    /// Empty: the project declares no data class bound.
    data_classes: BTreeSet<String>,
}

impl PrincipalBound {
    /// A validated bound.
    ///
    /// # Errors
    ///
    /// [`DelegationPolicyError::PrincipalBoundTooLarge`] for more toolkits,
    /// tools or credential references than the limits (or a name past its
    /// length), and the data class errors of [`data_classes`].
    pub fn new(
        toolkits: impl IntoIterator<Item = PrincipalToolkit>,
        credentials: Option<Vec<String>>,
        classes: impl IntoIterator<Item = String>,
    ) -> Result<Self, DelegationPolicyError> {
        let mut bound_toolkits = BTreeMap::new();
        for toolkit in toolkits {
            let selection = match toolkit.tools {
                None => ToolSelection::All {
                    excluded: BTreeSet::new(),
                },
                Some(tools) => {
                    if tools.len() > MAX_TOOL_NAMES
                        || tools
                            .iter()
                            .any(|tool| tool.len() > MAX_PRINCIPAL_TOOL_BYTES)
                    {
                        return Err(DelegationPolicyError::PrincipalBoundTooLarge);
                    }
                    ToolSelection::Only(tools)
                }
            };
            bound_toolkits.insert(toolkit.id, selection);
            // The limit is on distinct toolkits: a repeated id is not one more.
            if bound_toolkits.len() > MAX_PRINCIPAL_TOOLKITS {
                return Err(DelegationPolicyError::PrincipalBoundTooLarge);
            }
        }
        let credentials = match credentials {
            None => None,
            Some(references) => {
                let mut set = BTreeSet::new();
                for reference in references {
                    if reference.len() > MAX_PRINCIPAL_REFERENCE_BYTES {
                        return Err(DelegationPolicyError::PrincipalBoundTooLarge);
                    }
                    // Reference ids are UUIDs: compared without case, as the
                    // database spells them lower-case and a toolkit's
                    // settings may not.
                    set.insert(reference.to_ascii_lowercase());
                    if set.len() > MAX_PRINCIPAL_CREDENTIALS {
                        return Err(DelegationPolicyError::PrincipalBoundTooLarge);
                    }
                }
                Some(set)
            }
        };
        Ok(Self {
            toolkits: bound_toolkits,
            credentials,
            data_classes: data_classes(classes)?,
        })
    }

    /// Whether the principal may use the toolkit. A toolkit with no row (a
    /// built-in named by its type) is not bounded here.
    #[must_use]
    pub fn admits_toolkit(&self, id: Option<u64>) -> bool {
        id.is_none_or(|id| self.toolkits.contains_key(&id))
    }

    /// Whether a callee toolkit may name the credential reference.
    #[must_use]
    pub fn admits_credential(&self, reference: &str) -> bool {
        self.credentials
            .as_ref()
            .is_none_or(|references| references.contains(&reference.to_ascii_lowercase()))
    }

    /// Whether the principal may use a toolkit row as frozen: the toolkit
    /// itself and the credential reference its settings name.
    pub(crate) fn admits_reference(
        &self,
        tool_id: Option<u64>,
        settings: Option<&Map<String, Value>>,
    ) -> bool {
        if !self.admits_toolkit(tool_id) {
            return false;
        }
        let reference = settings.map(credential_reference_id).unwrap_or_default();
        reference.is_empty() || self.admits_credential(&reference)
    }

    /// The first thing `requested` (a callee's own grants) needs that the
    /// principal may not use, in the order of the caller check: toolkits,
    /// credentials, tools, data classes.
    fn refusal(
        &self,
        requested: &GrantSet,
        callee_classes: &BTreeSet<String>,
    ) -> Option<DelegationRefusal> {
        let refuse = |kind, name: &str, toolkit: Option<&str>| DelegationRefusal::PrincipalBound {
            kind,
            name: name.to_owned(),
            toolkit: toolkit.map(str::to_owned),
        };
        if let Some(wanted) = &requested.toolkits {
            for (key, grant) in wanted {
                if !self.admits_toolkit(toolkit_id(key)) {
                    return Some(refuse(GrantKind::Toolkit, &grant.label, None));
                }
            }
            for grant in wanted.values() {
                if let Some(credential) = &grant.credential
                    && !self.admits_credential(credential)
                {
                    return Some(refuse(
                        GrantKind::Credential,
                        credential,
                        Some(&grant.label),
                    ));
                }
            }
            for (key, grant) in wanted {
                let Some(allowed) = toolkit_id(key).and_then(|id| self.toolkits.get(&id)) else {
                    continue;
                };
                if let ToolSelection::Only(tools) = &grant.selection
                    && let Some(tool) = tools.iter().find(|tool| !allowed.allows(tool))
                {
                    return Some(refuse(GrantKind::Tool, tool, Some(&grant.label)));
                }
            }
        }
        if !self.data_classes.is_empty()
            && let Some(class) = callee_classes
                .iter()
                .find(|class| !self.data_classes.contains(*class))
        {
            return Some(refuse(GrantKind::DataClass, class, None));
        }
        None
    }
}

/// A grant set document that is not the canonical form.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct GrantSetError;

impl fmt::Display for GrantSetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the delegation grant set is not in its canonical form")
    }
}

impl std::error::Error for GrantSetError {}

fn string_list(values: &[Value], limit: usize) -> Result<Vec<String>, GrantSetError> {
    if values.len() > limit {
        return Err(GrantSetError);
    }
    values
        .iter()
        .map(|value| value.as_str().map(str::to_owned).ok_or(GrantSetError))
        .collect()
}

fn canonical_toolkit(entry: &Value) -> Result<(String, ToolkitGrant), GrantSetError> {
    let object = entry.as_object().ok_or(GrantSetError)?;
    let key = object
        .get("key")
        .and_then(Value::as_str)
        .filter(|key| key.starts_with("toolkit:") || key.starts_with("builtin:"))
        .ok_or(GrantSetError)?;
    let tools = object
        .get("tools")
        .and_then(Value::as_object)
        .filter(|tools| tools.len() == 1)
        .ok_or(GrantSetError)?;
    let (mode, names) = tools.iter().next().ok_or(GrantSetError)?;
    let names = names.as_array().ok_or(GrantSetError)?;
    let names: BTreeSet<String> = string_list(names, MAX_TOOL_NAMES)?.into_iter().collect();
    let selection = match mode.as_str() {
        "only" => ToolSelection::Only(names),
        "all_except" => ToolSelection::All { excluded: names },
        _ => return Err(GrantSetError),
    };
    let credential = match object.get("credential") {
        Some(Value::Null) | None => None,
        // No credential is null, never "": Main refuses the empty form too,
        // so both sides digest the same document.
        Some(Value::String(reference)) if !reference.is_empty() => Some(reference.clone()),
        Some(_) => return Err(GrantSetError),
    };
    Ok((
        key.to_owned(),
        ToolkitGrant {
            label: key.to_owned(),
            selection,
            credential,
        },
    ))
}

fn toolkit_grant(
    tool_id: Option<u64>,
    tool_type: &str,
    label: &str,
    settings: Option<&Map<String, Value>>,
) -> (String, ToolkitGrant) {
    let key = tool_id.map_or_else(
        || format!("builtin:{tool_type}"),
        |id| format!("toolkit:{id}"),
    );
    let names = |field: &str| -> BTreeSet<String> {
        settings
            .and_then(|settings| settings.get(field))
            .and_then(Value::as_array)
            .map(|names| {
                names
                    .iter()
                    .take(MAX_TOOL_NAMES)
                    .filter_map(Value::as_str)
                    .map(str::trim)
                    .filter(|name| !name.is_empty())
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default()
    };
    let selected = names("selected_tools");
    let selection = if selected.is_empty() {
        ToolSelection::All {
            excluded: names("excluded_tools"),
        }
    } else {
        ToolSelection::Only(selected)
    };
    let credential = settings
        .map(credential_reference_id)
        .filter(|reference| !reference.is_empty());
    (
        key,
        ToolkitGrant {
            label: label.to_owned(),
            selection,
            credential,
        },
    )
}

/// The toolkit's credential reference, as the governed toolkit client reads
/// it (top level, then a `*_configuration` object).
fn credential_reference_id(settings: &Map<String, Value>) -> String {
    crate::toolkits::governed::client::credential_reference_id(settings)
}

/// What a delegation step records.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DelegationEvidence {
    /// The callee's depth (the root is 0).
    pub depth: u32,
    /// The lowercase hex SHA-256 of the callee's effective grant set.
    pub effective_grant_digest: String,
    /// A refusal; under [`NarrowingMode::Report`] a grant refusal that was
    /// recorded and not enforced.
    pub refusal: Option<DelegationRefusal>,
    /// Whether the refusal (if any) was enforced.
    pub enforced: bool,
    /// The grant that bounded the delegation, and what it narrowed out.
    pub grant: Option<GrantEvidence>,
}

impl DelegationEvidence {
    /// The fields a delegation step's summary carries.
    #[must_use]
    pub fn summary(&self) -> Vec<(&'static str, Value)> {
        let mut fields = vec![
            ("delegation_depth", json!(self.depth)),
            ("effective_grant_digest", json!(self.effective_grant_digest)),
        ];
        if let Some(refusal) = &self.refusal {
            fields.push(("delegation_refusal", refusal.to_value()));
            fields.push(("delegation_enforced", json!(self.enforced)));
        }
        if let Some(grant) = &self.grant {
            fields.push(("delegation_grant_id", json!(grant.grant_id)));
            fields.push(("delegation_grant_digest", json!(grant.grant_digest)));
            if !grant.narrowed.is_empty() {
                fields.push((
                    "delegation_narrowed",
                    Value::Array(grant.narrowed.iter().map(Narrowed::to_value).collect()),
                ));
            }
        }
        fields
    }
}

/// A permitted delegation: the callee's own scope, for its callees, and the
/// evidence its step records.
#[derive(Clone, Debug)]
pub struct Delegation {
    pub scope: DelegationScope,
    pub evidence: DelegationEvidence,
}

/// A refused delegation: the refusal and the evidence its step records.
#[derive(Clone, Debug)]
pub struct RefusedDelegation {
    pub refusal: DelegationRefusal,
    pub evidence: DelegationEvidence,
}

/// One caller's place in the delegation chain: the run's policy, the
/// caller's effective grants and its depth.
#[derive(Clone, Debug)]
pub struct DelegationScope {
    policy: Arc<DelegationPolicy>,
    grants: Arc<GrantSet>,
    depth: u32,
    /// The saved agent or pipeline this scope belongs to: the caller of its
    /// delegations, the `Y` of an edge. 0 when it has none (an ad hoc root, or
    /// a callee reached without an edge).
    application_id: u64,
}

impl Default for DelegationScope {
    /// An ad hoc root under the default policy.
    fn default() -> Self {
        Self::root(Arc::default(), GrantSet::unconstrained())
    }
}

impl DelegationScope {
    /// The run's root (depth 0) with `grants`, limited to the policy's root
    /// data classes.
    #[must_use]
    pub fn root(policy: Arc<DelegationPolicy>, grants: GrantSet) -> Self {
        let grants = grants.with_data_classes(policy.root_data_classes());
        // The root is own ∩ P (ADR-0044 Amendment 1).
        let grants = match policy.principal() {
            Some(principal) => grants.bounded_by(principal),
            None => grants,
        };
        let application_id = policy.root_application_id;
        Self {
            policy,
            grants: Arc::new(grants),
            depth: 0,
            application_id,
        }
    }

    #[must_use]
    pub const fn depth(&self) -> u32 {
        self.depth
    }

    #[must_use]
    pub fn policy(&self) -> &DelegationPolicy {
        &self.policy
    }

    #[must_use]
    pub fn grants(&self) -> &GrantSet {
        &self.grants
    }

    /// A key that tells two scopes apart for caching: the depth, the agent the
    /// scope belongs to (its edges' grants depend on it) and the grant digest.
    #[must_use]
    pub fn cache_key(&self) -> String {
        format!(
            "{}:{}:{}",
            self.depth,
            self.application_id,
            self.grants.digest()
        )
    }

    fn next_depth(&self) -> Result<u32, DelegationRefusal> {
        let depth = self.depth.saturating_add(1);
        if depth > self.policy.max_depth {
            return Err(DelegationRefusal::DepthExceeded {
                depth,
                max_depth: self.policy.max_depth,
            });
        }
        Ok(depth)
    }

    /// Delegate to a saved agent or pipeline whose frozen tool list is
    /// `callee_tools` and whose registry declares `callee_data_classes`.
    ///
    /// Under [`NarrowingMode::Enforce`] a missing grant refuses. Otherwise
    /// `callee_tools` is narrowed in place (a toolkit that selects every tool
    /// is limited to the caller's selection; a toolkit left with no tool is
    /// removed) and the callee's scope is returned.
    ///
    /// # Errors
    ///
    /// [`RefusedDelegation`] for a delegation past the maximum depth, or,
    /// when enforcing, for a missing grant.
    pub fn delegate(
        &self,
        callee_tools: &mut Vec<Value>,
        callee_data_classes: &BTreeSet<String>,
        egress: &PublicPolicy,
    ) -> Result<Delegation, Box<RefusedDelegation>> {
        self.delegate_edge(None, callee_tools, callee_data_classes, egress)
    }

    /// [`Self::delegate`] across a known composition edge: `callee` is the id of
    /// the saved agent or pipeline the caller calls (ADR-0044 Amendment 1). When
    /// the run carries an approved grant for the edge, the callee is bounded by
    /// `own(X) ∩ (E(Y) ∪ G(Y→X)) ∩ P`: what it holds beyond that is **narrowed
    /// out** and recorded, and it still runs. An edge with no grant is
    /// delegated exactly as [`Self::delegate`] does, so no composition is
    /// refused by grants alone. The run principal's bound refuses first, in
    /// both modes, with or without a grant.
    ///
    /// # Errors
    ///
    /// [`RefusedDelegation`] for a delegation past the maximum depth, for a
    /// callee outside the run principal's bound, for a host off the run's
    /// egress allowlist when enforcing, and, when the edge has no grant and the
    /// run enforces, for a missing grant.
    pub fn delegate_edge(
        &self,
        callee: Option<u64>,
        callee_tools: &mut Vec<Value>,
        callee_data_classes: &BTreeSet<String>,
        egress: &PublicPolicy,
    ) -> Result<Delegation, Box<RefusedDelegation>> {
        let depth = match self.next_depth() {
            Ok(depth) => depth,
            Err(refusal) => {
                return Err(Box::new(RefusedDelegation {
                    evidence: DelegationEvidence {
                        depth: self.depth.saturating_add(1),
                        effective_grant_digest: self.grants.digest(),
                        refusal: Some(refusal.clone()),
                        enforced: true,
                        grant: None,
                    },
                    refusal,
                }));
            }
        };
        let requested = GrantSet::from_tools(callee_tools);
        // `∩ P`: the run principal's bound applies in both modes. `report`
        // never relaxes it.
        if let Some(refusal) = self
            .policy
            .principal()
            .and_then(|principal| principal.refusal(&requested, callee_data_classes))
        {
            return Err(Box::new(RefusedDelegation {
                evidence: DelegationEvidence {
                    depth,
                    effective_grant_digest: self.grants.digest(),
                    refusal: Some(refusal.clone()),
                    enforced: true,
                    grant: None,
                },
                refusal,
            }));
        }
        let hosts = declared_hosts(callee_tools);
        if let Some(callee_id) = callee
            && self.application_id != 0
            && let Some(grant) = self.policy.grant_between(self.application_id, callee_id)
        {
            return self.delegate_with_grant(
                (callee_id, grant),
                callee_tools,
                (&requested, callee_data_classes),
                (&hosts, egress),
                depth,
            );
        }
        let missing = self.missing(&requested, callee_data_classes, &hosts, egress);
        let enforce = self.policy.mode == NarrowingMode::Enforce;
        let effective = self.narrow(&requested, callee_data_classes, &hosts, egress);
        let evidence = DelegationEvidence {
            depth,
            effective_grant_digest: effective.digest(),
            refusal: missing.clone(),
            enforced: enforce && missing.is_some(),
            grant: None,
        };
        if let (Some(refusal), true) = (missing, enforce) {
            return Err(Box::new(RefusedDelegation { refusal, evidence }));
        }
        apply_selection(callee_tools, &effective);
        Ok(Delegation {
            scope: Self {
                policy: Arc::clone(&self.policy),
                grants: Arc::new(effective),
                depth,
                application_id: callee.unwrap_or(0),
            },
            evidence,
        })
    }

    /// The delegation of an edge that has an approved grant: the callee's
    /// effective set is `own(X) ∩ (E(Y) ∪ G) ∩ P`, narrowed in place, and what
    /// was narrowed out is recorded on the step with the grant's id and digest.
    /// The grant covers this edge only: the scope returned belongs to the
    /// callee, so its own edges need grants of their own.
    fn delegate_with_grant(
        &self,
        (callee_id, grant): (u64, &DelegationGrant),
        callee_tools: &mut Vec<Value>,
        (requested, callee_data_classes): (&GrantSet, &BTreeSet<String>),
        (hosts, egress): (&[(String, u16)], &PublicPolicy),
        depth: u32,
    ) -> Result<Delegation, Box<RefusedDelegation>> {
        let (mut effective, narrowed) =
            narrow_with_grant(&self.grants, grant.set(), requested, callee_data_classes);
        // `∩ P`, after the caller's set and the grant.
        if let Some(principal) = self.policy.principal() {
            effective = effective.bounded_by(principal);
        }
        // The run's egress allowlist bounds every callee, with or without a
        // grant (it is part of the principal's bound).
        let refusal = hosts
            .iter()
            .find(|(host, port)| !egress.admits(host, *port))
            .map(|(host, _)| DelegationRefusal::GrantMissing {
                kind: GrantKind::EgressHost,
                name: host.clone(),
                toolkit: None,
            });
        effective.egress_hosts = hosts
            .iter()
            .filter(|(host, port)| egress.admits(host, *port))
            .map(|(host, _)| host.clone())
            .collect();
        let enforce = self.policy.mode == NarrowingMode::Enforce;
        let evidence = DelegationEvidence {
            depth,
            effective_grant_digest: effective.digest(),
            refusal: refusal.clone(),
            enforced: enforce && refusal.is_some(),
            grant: Some(GrantEvidence {
                grant_id: grant.id(),
                grant_digest: grant.digest().to_owned(),
                narrowed,
            }),
        };
        if let (Some(refusal), true) = (refusal, enforce) {
            return Err(Box::new(RefusedDelegation { refusal, evidence }));
        }
        apply_selection(callee_tools, &effective);
        Ok(Delegation {
            scope: Self {
                policy: Arc::clone(&self.policy),
                grants: Arc::new(effective),
                depth,
                application_id: callee_id,
            },
            evidence,
        })
    }

    /// Start a sub-agent: a child of this caller offered only `filter`'s
    /// subset of the caller's tools (read-only, or an isolated checkout),
    /// never a tool in `excluded`. Its grants are a subset of the caller's
    /// by construction, so only the depth can refuse it.
    ///
    /// # Errors
    ///
    /// [`DelegationRefusal::DepthExceeded`] past the maximum depth.
    pub fn delegate_subagent(
        &self,
        filter: &str,
        excluded: &BTreeSet<String>,
    ) -> Result<DelegationEvidence, DelegationRefusal> {
        self.delegate_subagent_scope(filter, excluded)
            .map(|delegation| delegation.evidence)
    }

    /// [`Self::delegate_subagent`] with the child's own scope: one level
    /// deeper, with this caller's grants (a child's tools are a subset of
    /// them). A child that may start sub-agents of its own (ADR-0050
    /// decision 4, #1824) delegates from this scope.
    ///
    /// # Errors
    ///
    /// [`DelegationRefusal::DepthExceeded`] past the maximum depth.
    pub fn delegate_subagent_scope(
        &self,
        filter: &str,
        excluded: &BTreeSet<String>,
    ) -> Result<Delegation, DelegationRefusal> {
        let depth = self.next_depth()?;
        let digest = hex_sha256(&json!({
            "schema": "elitea.delegation.subagent.v1",
            "caller": self.grants.digest(),
            "filter": filter,
            "excluded": excluded,
        }));
        Ok(Delegation {
            scope: Self {
                policy: Arc::clone(&self.policy),
                grants: Arc::clone(&self.grants),
                depth,
                application_id: self.application_id,
            },
            evidence: DelegationEvidence {
                depth,
                effective_grant_digest: digest,
                refusal: None,
                enforced: false,
                grant: None,
            },
        })
    }

    /// The first grant `requested` needs that this caller does not hold, in
    /// a stable order: toolkits, credentials, tools, data classes, hosts.
    fn missing(
        &self,
        requested: &GrantSet,
        callee_classes: &BTreeSet<String>,
        hosts: &[(String, u16)],
        egress: &PublicPolicy,
    ) -> Option<DelegationRefusal> {
        let ceiling = self.caller_ceiling();
        if let (Some(held), Some(wanted)) = (&ceiling.toolkits, &requested.toolkits) {
            for (key, grant) in wanted {
                if !held.contains_key(key) {
                    return Some(DelegationRefusal::GrantMissing {
                        kind: GrantKind::Toolkit,
                        name: grant.label.clone(),
                        toolkit: None,
                    });
                }
            }
            let credentials = ceiling.credentials().unwrap_or_default();
            for grant in wanted.values() {
                if let Some(credential) = &grant.credential
                    && !credentials.contains(credential.as_str())
                {
                    return Some(DelegationRefusal::GrantMissing {
                        kind: GrantKind::Credential,
                        name: credential.clone(),
                        toolkit: Some(grant.label.clone()),
                    });
                }
            }
            for (key, grant) in wanted {
                let Some(caller_grant) = held.get(key) else {
                    continue;
                };
                if let ToolSelection::Only(tools) = &grant.selection
                    && let Some(tool) = tools
                        .iter()
                        .find(|tool| !caller_grant.selection.allows(tool))
                {
                    return Some(DelegationRefusal::GrantMissing {
                        kind: GrantKind::Tool,
                        name: tool.clone(),
                        toolkit: Some(grant.label.clone()),
                    });
                }
            }
        }
        if let Some(held) = &ceiling.data_classes
            && let Some(class) = callee_classes.iter().find(|class| !held.contains(*class))
        {
            return Some(DelegationRefusal::GrantMissing {
                kind: GrantKind::DataClass,
                name: class.clone(),
                toolkit: None,
            });
        }
        if let Some((host, _)) = hosts
            .iter()
            .find(|(host, port)| !egress.admits(host, *port))
        {
            return Some(DelegationRefusal::GrantMissing {
                kind: GrantKind::EgressHost,
                name: host.clone(),
                toolkit: None,
            });
        }
        None
    }

    /// The callee's effective grants: the intersection with this caller's.
    fn narrow(
        &self,
        requested: &GrantSet,
        callee_classes: &BTreeSet<String>,
        hosts: &[(String, u16)],
        egress: &PublicPolicy,
    ) -> GrantSet {
        let ceiling = self.caller_ceiling();
        let toolkits = match (&ceiling.toolkits, &requested.toolkits) {
            (Some(held), Some(wanted)) => Some(
                wanted
                    .iter()
                    .filter_map(|(key, grant)| {
                        let selection = held.get(key).map_or_else(
                            // Report mode only: the caller lacks it, and the
                            // callee keeps it unenforced.
                            || grant.selection.clone(),
                            |caller_grant| caller_grant.selection.intersect(&grant.selection),
                        );
                        if matches!(&selection, ToolSelection::Only(tools) if tools.is_empty()) {
                            return None;
                        }
                        Some((
                            key.clone(),
                            ToolkitGrant {
                                selection,
                                ..grant.clone()
                            },
                        ))
                    })
                    .collect(),
            ),
            (None, wanted) => wanted.clone(),
            (Some(held), None) => Some(held.clone()),
        };
        let data_classes = if callee_classes.is_empty() {
            ceiling.data_classes.clone()
        } else {
            Some(match &ceiling.data_classes {
                Some(held) => callee_classes.intersection(held).cloned().collect(),
                None => callee_classes.clone(),
            })
        };
        let egress_hosts = hosts
            .iter()
            .filter(|(host, port)| egress.admits(host, *port))
            .map(|(host, _)| host.clone())
            .collect();
        let effective = GrantSet {
            toolkits,
            data_classes,
            egress_hosts,
        };
        // `∩ P`, after the caller's set (and, once it lands, the edge grant).
        match self.policy.principal() {
            Some(principal) => effective.bounded_by(principal),
            None => effective,
        }
    }

    /// What this caller lets its callee hold: `E(Y)`, the caller's effective
    /// set. ADR-0044 Amendment 1 widens it to `E(Y) ∪ G(Y→X)`, where `G` is
    /// the approved, current delegation grant of the edge (story #2158); that
    /// union goes here, before [`Self::missing`] and [`Self::narrow`] read it
    /// and before `P` is applied. Until then `G` is empty and this is the
    /// caller's set.
    fn caller_ceiling(&self) -> &GrantSet {
        &self.grants
    }
}

/// Write the effective selections into the callee's frozen tools: a
/// toolkit narrowed to some tools gets `selected_tools`, one that keeps every
/// tool but more exclusions gets `excluded_tools`, one left with none is
/// removed.
fn apply_selection(tools: &mut Vec<Value>, effective: &GrantSet) {
    let Some(granted) = &effective.toolkits else {
        return;
    };
    tools.retain_mut(|tool| {
        let Some(object) = tool.as_object_mut() else {
            return true;
        };
        let tool_type = object
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_owned();
        if tool_type.is_empty() || tool_type == "application" {
            return true;
        }
        let key = object.get("id").and_then(Value::as_u64).map_or_else(
            || format!("builtin:{tool_type}"),
            |id| format!("toolkit:{id}"),
        );
        let Some(grant) = granted.get(&key) else {
            return false;
        };
        let Some(settings) = object.get_mut("settings").and_then(Value::as_object_mut) else {
            return true;
        };
        match &grant.selection {
            ToolSelection::Only(names) => {
                settings.insert("selected_tools".to_owned(), json!(names));
            }
            ToolSelection::All { excluded } if !excluded.is_empty() => {
                settings.insert("excluded_tools".to_owned(), json!(excluded));
            }
            ToolSelection::All { .. } => {}
        }
        true
    });
}

/// The hosts of the explicit URLs (`scheme://host`) a callee's toolkits
/// are configured with: a field named `url`, `endpoint`, `host`,
/// `api_base` or `server`, or ending in `_url`, `_endpoint` or `_host`, as
/// Main reads them for the run's allowlist. A value without a scheme, a
/// template and a non-HTTP scheme are not read here: Main may derive a
/// different host from them, and a guess must never refuse. The `openapi`
/// family is not read (its spec names hosts it never calls).
#[must_use]
pub fn declared_hosts(tools: &[Value]) -> Vec<(String, u16)> {
    let mut hosts = BTreeSet::new();
    for tool in tools.iter().take(MAX_TOOLKITS) {
        let Some(object) = tool.as_object() else {
            continue;
        };
        let tool_type = object.get("type").and_then(Value::as_str).unwrap_or("");
        if tool_type.is_empty() || tool_type == "application" || tool_type == "openapi" {
            continue;
        }
        if let Some(settings) = object.get("settings") {
            walk_hosts(settings, 0, &mut hosts);
        }
    }
    hosts.into_iter().take(MAX_DECLARED_HOSTS).collect()
}

fn walk_hosts(value: &Value, depth: usize, hosts: &mut BTreeSet<(String, u16)>) {
    if depth > MAX_HOST_WALK_DEPTH || hosts.len() >= MAX_DECLARED_HOSTS {
        return;
    }
    match value {
        Value::Object(object) => {
            for (key, child) in object {
                match child {
                    Value::String(text) if url_field(key) => {
                        if let Some(host) = explicit_url_host(text) {
                            hosts.insert(host);
                        }
                    }
                    Value::String(_) => {}
                    other => walk_hosts(other, depth + 1, hosts),
                }
            }
        }
        Value::Array(items) => {
            for item in items {
                walk_hosts(item, depth + 1, hosts);
            }
        }
        _ => {}
    }
}

fn url_field(key: &str) -> bool {
    let key = key.to_ascii_lowercase();
    matches!(
        key.as_str(),
        "url" | "endpoint" | "host" | "api_base" | "server"
    ) || key.ends_with("_url")
        || key.ends_with("_endpoint")
        || key.ends_with("_host")
}

fn explicit_url_host(text: &str) -> Option<(String, u16)> {
    let text = text.trim();
    if !text.contains("://") || text.len() > 2_048 || text.contains(['{', '}', '$', '%']) {
        return None;
    }
    let url = reqwest::Url::parse(text).ok()?;
    if !matches!(url.scheme(), "http" | "https" | "ws" | "wss") {
        return None;
    }
    let host = url.host_str()?.to_ascii_lowercase();
    let port = url.port_or_known_default()?;
    Some((host, port))
}

pub(crate) fn hex_sha256(value: &Value) -> String {
    let bytes = crate::canonical::to_vec(value).unwrap_or_default();
    digest::digest(&digest::SHA256, &bytes)
        .as_ref()
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            use std::fmt::Write as _;
            let _ = write!(out, "{byte:02x}");
            out
        })
}

#[cfg(test)]
#[path = "delegation_tests.rs"]
mod tests;

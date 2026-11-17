//! Delegation grants (ADR-0044 Amendment 1, story #2166).
//!
//! Attaching a callee X to a caller Y creates a grant `G(Y→X)` for that edge:
//! the set X held when the grant was approved, pinned to its digest. Main sends
//! the approved grants of the run's closure on the delegation policy. The
//! effective set of X is
//!
//! ```text
//! E(X) = own(X) ∩ ( E(Y) ∪ G(Y→X) ) ∩ P
//! ```
//!
//! This module is the `∪ G` term and the narrowing it implies; the principal
//! bound `∩ P` is applied by the caller of [`narrow_with_grant`] (see
//! `DelegationScope::delegate_edge`).
//!
//! The check is set containment, not version equality. What X holds beyond
//! `E(Y) ∪ G` is **narrowed out** and recorded; X still runs. A newer version
//! of X whose set is the same or smaller loses nothing. The grant covers the
//! edge only: it does not cover X's own callees, whose edges have grants of
//! their own, and the depth cap still applies.
//!
//! Grants take effect only while Main's switch is on
//! (`WorkerDelegationPolicyV1.grants_enforced`). Without it, or for an edge
//! with no grant, delegation is exactly what it was before grants.

use std::collections::{BTreeMap, BTreeSet};

use serde_json::{Value, json};

use super::{DelegationPolicyError, GrantKind, GrantSet, ToolSelection, ToolkitGrant};

/// The most grants one policy carries (Main's bound).
pub const MAX_GRANT_EDGES: usize = 256;

/// How the callee is attached to the caller.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum EdgeKind {
    SubAgent,
    AgentTool,
    PipelineChild,
    PipelineNode,
}

impl EdgeKind {
    /// The kind a grant names (`sub_agent`, `agent_tool`, `pipeline_child` or
    /// `pipeline_node`).
    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "sub_agent" => Some(Self::SubAgent),
            "agent_tool" => Some(Self::AgentTool),
            "pipeline_child" => Some(Self::PipelineChild),
            "pipeline_node" => Some(Self::PipelineNode),
            _ => None,
        }
    }

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::SubAgent => "sub_agent",
            Self::AgentTool => "agent_tool",
            Self::PipelineChild => "pipeline_child",
            Self::PipelineNode => "pipeline_node",
        }
    }
}

/// One composition edge: the saved agent or pipeline that calls, the one it
/// calls and how.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct EdgeKey {
    pub caller: u64,
    pub callee: u64,
    pub kind: EdgeKind,
}

/// An approved delegation grant, pinned to its digest.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DelegationGrant {
    id: u64,
    digest: String,
    set: GrantSet,
}

impl DelegationGrant {
    /// A grant whose set hashes to `digest`.
    ///
    /// # Errors
    ///
    /// [`DelegationPolicyError::MalformedGrant`] when the set does not hash to
    /// the digest Main sent: a grant is read as approved only if it is the
    /// one that was approved.
    pub fn new(id: u64, digest: String, set: GrantSet) -> Result<Self, DelegationPolicyError> {
        if id == 0 || set.digest() != digest {
            return Err(DelegationPolicyError::MalformedGrant);
        }
        Ok(Self { id, digest, set })
    }

    #[must_use]
    pub const fn id(&self) -> u64 {
        self.id
    }

    #[must_use]
    pub fn digest(&self) -> &str {
        &self.digest
    }

    #[must_use]
    pub const fn set(&self) -> &GrantSet {
        &self.set
    }
}

/// The grants of a run, by edge.
pub type Grants = BTreeMap<EdgeKey, DelegationGrant>;

/// What the grant of an edge did to a delegation: which grant it was and what
/// the callee held that `E(Y) ∪ G` did not cover.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GrantEvidence {
    pub grant_id: u64,
    pub grant_digest: String,
    /// What was narrowed out of the callee for this run, empty when the grant
    /// and the caller cover everything it holds.
    pub narrowed: Vec<Narrowed>,
}

/// One thing narrowed out of a callee.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct Narrowed {
    pub kind: GrantKind,
    pub name: String,
    pub toolkit: Option<String>,
}

impl Narrowed {
    /// The structured form a step record carries.
    #[must_use]
    pub fn to_value(&self) -> Value {
        let mut value = json!({"kind": self.kind.as_str(), "name": self.name});
        if let Some(toolkit) = &self.toolkit {
            value["toolkit"] = json!(toolkit);
        }
        value
    }
}

impl ToolSelection {
    /// The union of two selections: what either covers.
    fn union(&self, other: &Self) -> Self {
        match (self, other) {
            (Self::All { excluded: a }, Self::All { excluded: b }) => Self::All {
                excluded: a.intersection(b).cloned().collect(),
            },
            (Self::All { excluded }, Self::Only(tools))
            | (Self::Only(tools), Self::All { excluded }) => Self::All {
                excluded: excluded.difference(tools).cloned().collect(),
            },
            (Self::Only(a), Self::Only(b)) => Self::Only(a.union(b).cloned().collect()),
        }
    }
}

/// The callee's effective grants through `grant` and the caller `caller`, and
/// what was narrowed out: `own(X) ∩ (E(Y) ∪ G)` for toolkits, tools,
/// credentials and data classes (`requested` is `own(X)`, `callee_classes` its
/// declared data classes).
///
/// Egress hosts are not narrowed here: the run's allowlist bounds them for
/// every callee (the principal bound), and the caller checks that.
#[must_use]
pub fn narrow_with_grant(
    caller: &GrantSet,
    grant: &GrantSet,
    requested: &GrantSet,
    callee_classes: &BTreeSet<String>,
) -> (GrantSet, Vec<Narrowed>) {
    let mut narrowed = Vec::new();
    let toolkits = requested
        .toolkits
        .as_ref()
        .map(|wanted| narrow_toolkits(caller, grant, wanted, &mut narrowed));
    let data_classes = narrow_classes(caller, grant, callee_classes, &mut narrowed);
    narrowed.sort();
    (
        GrantSet {
            toolkits: toolkits.or_else(|| caller.toolkits.clone()),
            data_classes,
            egress_hosts: BTreeSet::new(),
        },
        narrowed,
    )
}

fn narrow_toolkits(
    caller: &GrantSet,
    grant: &GrantSet,
    wanted: &BTreeMap<String, ToolkitGrant>,
    narrowed: &mut Vec<Narrowed>,
) -> BTreeMap<String, ToolkitGrant> {
    // A caller that constrains no toolkit (an ad hoc root) holds everything.
    let caller_credentials = caller.credentials();
    let mut kept = BTreeMap::new();
    for (key, want) in wanted {
        let held = held_selection(caller, grant, key);
        let Some(held) = held else {
            narrowed.push(Narrowed {
                kind: GrantKind::Toolkit,
                name: want.label.clone(),
                toolkit: None,
            });
            continue;
        };
        if let Some(credential) = &want.credential
            && !credential_held(caller_credentials.as_ref(), grant, key, credential)
        {
            narrowed.push(Narrowed {
                kind: GrantKind::Credential,
                name: credential.clone(),
                toolkit: Some(want.label.clone()),
            });
            continue;
        }
        let selection = held.intersect(&want.selection);
        record_cut_tools(&want.selection, &selection, &want.label, narrowed);
        if matches!(&selection, ToolSelection::Only(tools) if tools.is_empty()) {
            continue;
        }
        kept.insert(
            key.clone(),
            ToolkitGrant {
                selection,
                ..want.clone()
            },
        );
    }
    kept
}

/// What `E(Y) ∪ G` lets the toolkit use: the union of the caller's selection
/// and the grant's, or `None` when neither holds the toolkit. A caller that
/// constrains no toolkit holds every tool of every toolkit.
fn held_selection(caller: &GrantSet, grant: &GrantSet, key: &str) -> Option<ToolSelection> {
    let everything = || ToolSelection::All {
        excluded: BTreeSet::new(),
    };
    let from_caller = match &caller.toolkits {
        None => Some(everything()),
        Some(held) => held.get(key).map(|toolkit| toolkit.selection.clone()),
    };
    let from_grant = grant
        .toolkits
        .as_ref()
        .and_then(|held| held.get(key))
        .map(|toolkit| toolkit.selection.clone());
    match (from_caller, from_grant) {
        (Some(a), Some(b)) => Some(a.union(&b)),
        (Some(only), None) | (None, Some(only)) => Some(only),
        (None, None) => None,
    }
}

/// Whether the callee toolkit may name the credential reference: the caller
/// holds it (or constrains no toolkit), or the grant names it on this toolkit.
fn credential_held(
    caller_credentials: Option<&BTreeSet<&str>>,
    grant: &GrantSet,
    key: &str,
    credential: &str,
) -> bool {
    let from_caller = caller_credentials.is_none_or(|held| held.contains(credential));
    let from_grant = grant
        .toolkits
        .as_ref()
        .and_then(|held| held.get(key))
        .and_then(|toolkit| toolkit.credential.as_deref())
        == Some(credential);
    from_caller || from_grant
}

/// Record the tools the cut removed: each tool of an `only` selection that is
/// not left, or, for a selection of every tool cut to some, one `*` item.
fn record_cut_tools(
    wanted: &ToolSelection,
    kept: &ToolSelection,
    label: &str,
    narrowed: &mut Vec<Narrowed>,
) {
    let item = |name: &str| Narrowed {
        kind: GrantKind::Tool,
        name: name.to_owned(),
        toolkit: Some(label.to_owned()),
    };
    match (wanted, kept) {
        (ToolSelection::Only(wanted), kept) => {
            narrowed.extend(
                wanted
                    .iter()
                    .filter(|tool| !kept.allows(tool.as_str()))
                    .map(|tool| item(tool.as_str())),
            );
        }
        (ToolSelection::All { .. }, ToolSelection::Only(_)) => narrowed.push(item("*")),
        (ToolSelection::All { excluded: wanted }, ToolSelection::All { excluded: kept }) => {
            narrowed.extend(kept.difference(wanted).map(|tool| item(tool.as_str())));
        }
    }
}

/// The data classes the callee keeps: those `E(Y) ∪ G` covers. A callee that
/// declares none inherits the caller's, as before grants.
fn narrow_classes(
    caller: &GrantSet,
    grant: &GrantSet,
    callee_classes: &BTreeSet<String>,
    narrowed: &mut Vec<Narrowed>,
) -> Option<BTreeSet<String>> {
    if callee_classes.is_empty() {
        return caller.data_classes.clone();
    }
    let Some(held) = &caller.data_classes else {
        return Some(callee_classes.clone());
    };
    let granted = grant.data_classes.as_ref();
    let mut kept = BTreeSet::new();
    for class in callee_classes {
        if held.contains(class) || granted.is_some_and(|set| set.contains(class)) {
            kept.insert(class.clone());
        } else {
            narrowed.push(Narrowed {
                kind: GrantKind::DataClass,
                name: class.clone(),
                toolkit: None,
            });
        }
    }
    Some(kept)
}

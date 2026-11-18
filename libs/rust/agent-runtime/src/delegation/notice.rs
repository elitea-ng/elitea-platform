//! The notice a refused or narrowed delegation shows the person (ADR-0044
//! Amendment 1, A1.2 rule 5).
//!
//! The model keeps reading the typed refusal as the tool result. This is the
//! same fact for the chat: a `delegation_refused` or `delegation_narrowed`
//! node event whose `response_metadata.delegation_v1` names the edge, why and
//! what was missing. It carries names and ids only: no credential value, no
//! tool argument. Main stamps `can_review` and the editors' names on the way
//! out, per viewer.

use serde_json::{Value, json};

use super::{DEPTH_EXCEEDED, DelegationEvidence, DelegationRefusal, Narrowed};

/// The node event type of a refused delegation.
pub const REFUSED_EVENT: &str = "delegation_refused";
/// The node event type of a delegation that ran with less than the callee
/// holds, because its grant no longer covers it.
pub const NARROWED_EVENT: &str = "delegation_narrowed";
/// The `response_metadata` member that carries the notice.
pub const NOTICE_METADATA_KEY: &str = "delegation_v1";
/// The most missing grants a notice names; the step record keeps them all.
pub const MAX_NOTICE_ITEMS: usize = 16;

/// What the projector knows about the delegation's place in the run.
#[derive(Clone, Copy, Debug)]
pub struct NoticeContext<'a> {
    /// The tool call the delegation answers; the notice sits at its place.
    pub call_id: &'a str,
    /// The callee's display name.
    pub callee_name: &'a str,
    /// The caller's display name, when it is known.
    pub caller_name: Option<&'a str>,
    /// `agent_tool` for a saved agent, `pipeline_child` for a saved pipeline.
    pub edge_kind: &'a str,
}

/// One notice: the node event type and its `delegation_v1` value.
#[derive(Clone, Debug, PartialEq)]
pub struct DelegationNotice {
    pub event: &'static str,
    pub value: Value,
}

impl DelegationEvidence {
    /// The notice this delegation shows the person, or `None` when it was
    /// neither refused nor narrowed. A refusal `report` mode recorded and did
    /// not enforce is not shown: the delegation ran.
    #[must_use]
    pub fn notice(&self, context: &NoticeContext<'_>) -> Option<DelegationNotice> {
        let (event, mut value) =
            if let Some(refusal) = self.refusal.as_ref().filter(|_| self.enforced) {
                (REFUSED_EVENT, refusal_body(refusal))
            } else {
                let grant = self
                    .grant
                    .as_ref()
                    .filter(|grant| !grant.narrowed.is_empty())?;
                (NARROWED_EVENT, narrowed_body(&grant.narrowed))
            };
        let mut edge = json!({
            "callee_name": context.callee_name,
            "kind": context.edge_kind,
        });
        if let Some(found) = self.edge {
            if found.caller != 0 {
                edge["caller_id"] = json!(found.caller);
            }
            edge["callee_id"] = json!(found.callee);
        }
        if let Some(caller) = context.caller_name {
            edge["caller_name"] = json!(caller);
        }
        value["id"] = json!(context.call_id);
        value["edge"] = edge;
        if let Some(grant) = &self.grant {
            value["grant_id"] = json!(grant.grant_id);
        }
        Some(DelegationNotice { event, value })
    }
}

fn refusal_body(refusal: &DelegationRefusal) -> Value {
    let mut body = json!({"kind": "refused", "code": refusal.code()});
    match refusal {
        DelegationRefusal::GrantMissing {
            kind,
            name,
            toolkit,
        } => {
            body["reason"] = json!("no_grant");
            body["missing"] = json!([item(kind.as_str(), name, toolkit.as_deref())]);
        }
        DelegationRefusal::PrincipalBound {
            kind,
            name,
            toolkit,
        } => {
            body["reason"] = json!("principal");
            body["missing"] = json!([item(kind.as_str(), name, toolkit.as_deref())]);
        }
        DelegationRefusal::DepthExceeded { depth, max_depth } => {
            debug_assert_eq!(refusal.code(), DEPTH_EXCEEDED);
            body["reason"] = json!("depth");
            body["missing"] = json!([]);
            body["depth"] = json!(depth);
            body["max_depth"] = json!(max_depth);
        }
    }
    body
}

/// A narrowing is drift: the callee holds something its approved grant (and
/// its caller) do not cover, so the run left it out.
fn narrowed_body(narrowed: &[Narrowed]) -> Value {
    json!({
        "kind": "narrowed",
        "reason": "stale",
        "missing": narrowed
            .iter()
            .take(MAX_NOTICE_ITEMS)
            .map(|entry| item(entry.kind.as_str(), &entry.name, entry.toolkit.as_deref()))
            .collect::<Vec<_>>(),
        "missing_total": narrowed.len(),
    })
}

fn item(kind: &str, name: &str, toolkit: Option<&str>) -> Value {
    let mut value = json!({"kind": kind, "name": name});
    if let Some(toolkit) = toolkit {
        value["toolkit"] = json!(toolkit);
    }
    value
}

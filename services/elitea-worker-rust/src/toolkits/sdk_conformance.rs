//! The SDK schema gate (ADR-0027 decision 8): a Rust toolkit family must
//! stay callable by what was saved against the Python SDK's catalogue.
//!
//! The SDK is the schema source of truth until the Python worker is gone:
//! `services/elitea-main/internal/runtimecomposition/current_toolkit_schema_snapshot.json`
//! is generated from its registry (`scripts/contract/sync_toolkit_schema_snapshot.py`).
//! Saved agent versions store tool NAMES, and pipeline nodes store argument
//! mappings written against the SDK's argument schemas, so for every tool a
//! family serves:
//!
//! * `unknown-tool` — the name must exist in the SDK type: a Rust-only name
//!   cannot have been selected by anything saved, and usually is a typo that
//!   leaves the SDK tool of the right name unserved;
//! * `drops:<p>` — every SDK property must be accepted: with
//!   `additionalProperties: false` a property the Rust schema does not list
//!   is a refused call for a pipeline that passes it;
//! * `requires:<p>` — the Rust schema may require only what the SDK
//!   requires: a mapping written against an SDK-optional argument omits it;
//! * `type:<p>` — a property both declare must admit a common JSON type
//!   (a Rust `number` accepts an SDK `integer`, not the other way round).
//!
//! Stricter bounds (`minimum`, `maxLength`, …) are allowed: they refuse a
//! value, not a call shape. A known, deliberate difference is an entry in
//! `sdk_conformance_exemptions.json` with its reason; an exemption that no
//! longer matches anything fails too, so the list cannot rot.

#![cfg(test)]

use adk_rust::Tool;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, OnceLock};

const SNAPSHOT: &str = include_str!(
    "../../../elitea-main/internal/runtimecomposition/current_toolkit_schema_snapshot.json"
);
const EXEMPTIONS: &str = include_str!("sdk_conformance_exemptions.json");
/// What elitea-main is told this worker serves: per type, and per TOOL for a
/// family that serves fewer tools than its SDK type declares.
const CAPABILITY: &str = include_str!(
    "../../../elitea-main/internal/runtimecomposition/current_rust_worker_toolkit_capability_snapshot.json"
);

/// The stored toolkit type of an SDK type: the Kubernetes family is stored
/// as `k8s` and published as `kubernetes`.
fn stored_type(sdk_type: &str) -> &str {
    match sdk_type {
        "kubernetes" => "k8s",
        other => other,
    }
}

/// The capability file's tool list for a stored type, if it has one.
fn declared_tools(stored: &str) -> Option<BTreeSet<String>> {
    let document: Value = serde_json::from_str(CAPABILITY).expect("worker capability JSON");
    document.get("supported_tools")?.get(stored).map(|names| {
        names
            .as_array()
            .expect("a tool list")
            .iter()
            .map(|name| name.as_str().expect("a tool name").to_owned())
            .collect()
    })
}

fn snapshot() -> &'static BTreeMap<String, Map<String, Value>> {
    static ENTRIES: OnceLock<BTreeMap<String, Map<String, Value>>> = OnceLock::new();
    ENTRIES.get_or_init(|| {
        let document: Value = serde_json::from_str(SNAPSHOT).expect("SDK schema snapshot");
        document["entries"]
            .as_array()
            .expect("snapshot entries")
            .iter()
            .map(|entry| {
                let schemas = entry["args_schemas"]
                    .as_object()
                    .cloned()
                    .unwrap_or_default();
                (
                    entry["type"].as_str().expect("entry type").to_owned(),
                    schemas,
                )
            })
            .collect()
    })
}

fn exemptions() -> &'static BTreeMap<String, String> {
    static EXEMPT: OnceLock<BTreeMap<String, String>> = OnceLock::new();
    EXEMPT.get_or_init(|| {
        let document: Value = serde_json::from_str(EXEMPTIONS).expect("exemptions JSON");
        document["exemptions"]
            .as_object()
            .expect("exemptions object")
            .iter()
            .map(|(key, reason)| {
                let reason = reason.as_str().expect("an exemption names its reason");
                assert!(
                    reason.len() >= 20,
                    "{key}: an exemption needs a real reason"
                );
                (key.clone(), reason.to_owned())
            })
            .collect()
    })
}

/// The JSON types a schema admits; empty means any.
fn types(schema: &Value) -> BTreeSet<String> {
    let mut out = BTreeSet::new();
    match schema.get("type") {
        Some(Value::String(kind)) => {
            out.insert(kind.clone());
        }
        Some(Value::Array(kinds)) => {
            out.extend(kinds.iter().filter_map(Value::as_str).map(str::to_owned));
        }
        _ => {}
    }
    for key in ["anyOf", "oneOf"] {
        if let Some(Value::Array(members)) = schema.get(key) {
            for member in members {
                let member_types = types(member);
                if member_types.is_empty() {
                    return BTreeSet::new();
                }
                out.extend(member_types);
            }
        }
    }
    out
}

/// Whether some value the SDK schema admits is admitted by the Rust one.
fn compatible(sdk: &BTreeSet<String>, rust: &BTreeSet<String>) -> bool {
    if sdk.is_empty() || rust.is_empty() {
        return true;
    }
    sdk.iter().any(|kind| {
        rust.contains(kind)
            || (kind == "integer" && rust.contains("number"))
            || (kind == "null" && sdk.len() > 1)
    })
}

fn properties(schema: &Value) -> Map<String, Value> {
    schema
        .get("properties")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}

fn required(schema: &Value) -> BTreeSet<String> {
    schema
        .get("required")
        .and_then(Value::as_array)
        .map(|names| {
            names
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default()
}

/// Every tool name the SDK type declares, for a family test that selects
/// tools by name: select them all and let the family keep what it serves.
pub(crate) fn sdk_tool_names(sdk_type: &str) -> Vec<String> {
    snapshot()
        .get(sdk_type)
        .unwrap_or_else(|| panic!("the SDK snapshot has no toolkit type {sdk_type:?}"))
        .keys()
        .cloned()
        .collect()
}

/// The violations of one family against the SDK type `sdk_type`, as
/// `sdk_type/tool/rule` keys.
pub(crate) fn violations(sdk_type: &str, tools: &[Arc<dyn Tool>]) -> BTreeSet<String> {
    let sdk = snapshot()
        .get(sdk_type)
        .unwrap_or_else(|| panic!("the SDK snapshot has no toolkit type {sdk_type:?}"));
    let mut found = BTreeSet::new();
    for tool in tools {
        let name = tool.name();
        let key = |rule: &str| format!("{sdk_type}/{name}/{rule}");
        let Some(sdk_schema) = sdk.get(name) else {
            found.insert(key("unknown-tool"));
            continue;
        };
        let rust_schema = tool.parameters_schema().unwrap_or(Value::Null);
        let closed = rust_schema.get("additionalProperties") == Some(&Value::Bool(false));
        let rust_properties = properties(&rust_schema);
        let sdk_properties = properties(sdk_schema);
        for (property, sdk_property) in &sdk_properties {
            match rust_properties.get(property) {
                None if closed => {
                    found.insert(key(&format!("drops:{property}")));
                }
                Some(rust_property) if !compatible(&types(sdk_property), &types(rust_property)) => {
                    found.insert(key(&format!("type:{property}")));
                }
                _ => {}
            }
        }
        let sdk_required = required(sdk_schema);
        for property in required(&rust_schema) {
            if !sdk_required.contains(&property) {
                found.insert(key(&format!("requires:{property}")));
            }
        }
    }
    found
}

/// Assert that `tools` — EVERY tool the family serves, unfiltered — keep the
/// SDK contract of `sdk_type`, apart from the family's listed exemptions,
/// and that each of those exemptions still matches something.
pub(crate) fn assert_sdk_conformance(sdk_type: &str, tools: &[Arc<dyn Tool>]) {
    assert!(!tools.is_empty(), "{sdk_type}: no tools to check");
    let found = violations(sdk_type, tools);
    let prefix = format!("{sdk_type}/");
    let listed: BTreeSet<&String> = exemptions()
        .keys()
        .filter(|key| key.starts_with(&prefix))
        .collect();
    let unexplained: Vec<&String> = found.iter().filter(|key| !listed.contains(key)).collect();
    let stale: Vec<&&String> = listed.iter().filter(|key| !found.contains(**key)).collect();
    assert!(
        unexplained.is_empty(),
        "{sdk_type}: the Rust tools break the SDK contract (fix them, or add a reasoned entry to sdk_conformance_exemptions.json): {unexplained:#?}"
    );
    assert!(
        stale.is_empty(),
        "{sdk_type}: these exemptions no longer match anything; remove them: {stale:#?}"
    );

    // The per-tool capability elitea-main reads (supported_tools) must be
    // exactly what the family serves: absent when it serves every SDK tool,
    // else the served names, so the catalogue marks the rest unavailable and
    // a direct call to one is refused before it is admitted.
    let served: BTreeSet<String> = tools.iter().map(|tool| tool.name().to_owned()).collect();
    let declared: BTreeSet<String> = snapshot()[sdk_type].keys().cloned().collect();
    let stored = stored_type(sdk_type);
    let listed = declared_tools(stored);
    if served.is_superset(&declared) {
        assert!(
            listed.is_none(),
            "{sdk_type}: the family serves every SDK tool, so supported_tools.{stored} must be absent from current_rust_worker_toolkit_capability_snapshot.json"
        );
    } else {
        assert_eq!(
            listed.as_ref(),
            Some(&served),
            "{sdk_type}: serves {} of {} SDK tools; current_rust_worker_toolkit_capability_snapshot.json supported_tools.{stored} must list exactly {served:?}",
            served.len(),
            declared.len()
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn types_follow_any_of_and_compatibility_is_directional() {
        let optional_int = json!({"anyOf": [{"type": "integer"}, {"type": "null"}]});
        assert_eq!(
            types(&optional_int),
            BTreeSet::from(["integer".to_owned(), "null".to_owned()])
        );
        let set = |kinds: &[&str]| {
            kinds
                .iter()
                .map(|k| (*k).to_owned())
                .collect::<BTreeSet<_>>()
        };
        assert!(compatible(&set(&["integer"]), &set(&["number"])));
        assert!(!compatible(&set(&["number"]), &set(&["integer"])));
        assert!(compatible(&set(&["integer", "null"]), &set(&["integer"])));
        assert!(!compatible(&set(&["string"]), &set(&["integer"])));
        assert!(compatible(&set(&[]), &set(&["integer"])));
    }

    #[test]
    fn every_exemption_names_a_snapshot_type() {
        for key in exemptions().keys() {
            let sdk_type = key.split('/').next().unwrap_or_default();
            assert!(snapshot().contains_key(sdk_type), "{key}: no such SDK type");
        }
    }
}

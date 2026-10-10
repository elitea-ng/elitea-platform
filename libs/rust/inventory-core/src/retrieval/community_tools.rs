//! The community tools (`InventoryRetrievalApiWrapper.list_communities`,
//! `get_community_detail`, `find_entity_community`,
//! `search_within_community`), over the `community_data` the communities
//! stage stored in `_metadata` (`crate::communities`).
//!
//! None of them is in a family table (`crate::tools`): the Python chat
//! agent added them, when the graph had communities, as closures for
//! `inventory_chat` / `investigate`. So [`handle`] serves nothing and the
//! functions here return the text those tools returned, for `investigate`
//! to call ([`has_communities`] is the condition the agent offered them
//! under).

// The answers are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use super::admin_tools::tables;
use super::pattern::lexical;
use super::semantic::location_and_description;
use super::view::GraphView;
use super::{Call, Handled};
use elitea_engine_core::pyvalue::{py_str, py_truthy};
use serde_json::{Map, Value};
use std::collections::HashSet;

/// See [`super::dispatch`]: no routed tool is a community tool.
#[must_use]
pub fn handle(_call: &Call<'_>) -> Handled {
    None
}

/// `_metadata.community_data`, when there is any.
fn community_data(view: &GraphView) -> Option<&Map<String, Value>> {
    view.graph
        .metadata
        .get("community_data")
        .and_then(Value::as_object)
        .filter(|data| !data.is_empty())
}

fn communities(view: &GraphView) -> Option<&Map<String, Value>> {
    community_data(view)?
        .get("communities")
        .and_then(Value::as_object)
}

/// Whether the graph has communities (`_has_communities`): the agent
/// offered the community tools only then.
#[must_use]
pub fn has_communities(view: &GraphView) -> bool {
    community_data(view).is_some()
}

/// `TYPE_PRIORITY.get(type.lower(), 0)`.
fn priority(kind: &str) -> i64 {
    tables()
        .community_type_priority
        .get(&kind.to_lowercase())
        .copied()
        .unwrap_or(0)
}

fn items(value: Option<&Value>) -> &[Value] {
    value.and_then(Value::as_array).map_or(&[], Vec::as_slice)
}

/// `info.get(key, default)` as `str()`.
fn text_or(info: &Map<String, Value>, key: &str, default: &str) -> String {
    info.get(key).map_or_else(|| default.to_owned(), py_str)
}

/// A number formatted as `format(value, '.{digits}f')`; `None` for a
/// value Python could not format so.
fn fixed(value: &Value, digits: usize) -> Option<String> {
    match value {
        Value::Bool(flag) => Some(format!("{:.digits$}", u8::from(*flag))),
        Value::Number(number) => number.as_f64().map(|n| format!("{n:.digits$}")),
        _ => None,
    }
}

/// The `TypeError` Python raised formatting a non-number with `.Nf`.
fn format_error(value: &Value) -> String {
    let kind = match value {
        Value::Null => "NoneType",
        Value::String(_) => return "Unknown format code 'f' for object of type 'str'".to_owned(),
        Value::Array(_) => "list",
        Value::Object(_) => "dict",
        Value::Bool(_) | Value::Number(_) => "float",
    };
    format!("unsupported format string passed to {kind}.__format__")
}

/// The chat agent's `list_communities` tool (`top_n`: the largest only).
#[must_use]
pub fn list_communities(view: &GraphView, top_n: Option<usize>) -> String {
    let (Some(data), Some(all)) = (community_data(view), communities(view)) else {
        return "No communities detected in this graph.".to_owned();
    };
    if all.is_empty() {
        return "No communities detected in this graph.".to_owned();
    }
    let size_of = |info: &Map<String, Value>| -> Value {
        info.get("stats")
            .and_then(Value::as_object)
            .and_then(|stats| stats.get("size"))
            .cloned()
            .unwrap_or_else(|| Value::from(items(info.get("members")).len()))
    };
    let empty = Map::new();
    let mut sorted: Vec<(&String, &Map<String, Value>)> = all
        .iter()
        .map(|(id, info)| (id, info.as_object().unwrap_or(&empty)))
        .collect();
    sorted.sort_by(|(_, a), (_, b)| {
        let size = |info| size_of(info).as_f64().unwrap_or(0.0);
        size(b).total_cmp(&size(a))
    });
    if let Some(top_n) = top_n.filter(|n| *n > 0) {
        sorted.truncate(top_n);
    }
    let algorithm = data
        .get("algorithm")
        .map_or_else(|| "None".to_owned(), py_str);
    let modularity = data.get("modularity").unwrap_or(&Value::Null);
    let Some(modularity) = fixed(modularity, 3) else {
        return format!("Error listing communities: {}", format_error(modularity));
    };
    let mut output = format!(
        "# Communities ({} detected, algorithm={algorithm}, modularity={modularity})\n\n",
        text_or(data, "num_communities", "0")
    );
    for (id, info) in sorted {
        let label = text_or(info, "label", "");
        let centroid_names: Vec<String> = items(info.get("centroids"))
            .iter()
            .take(3)
            .map(|centroid| centroid.get("name").map_or_else(String::new, py_str))
            .collect();
        output += &format!("## {id}: {label}\n");
        output += &format!("- **Size**: {} entities\n", py_str(&size_of(info)));
        let centroid_names = centroid_names.join(", ");
        if !centroid_names.is_empty() {
            output += &format!("- **Key entities**: {centroid_names}\n");
        }
        let types: Vec<String> = items(info.get("dominant_types"))
            .iter()
            .take(3)
            .map(py_str)
            .collect();
        if !types.is_empty() {
            output += &format!("- **Types**: {}\n", types.join(", "));
        }
        if let Some(summary) = info.get("summary").filter(|s| py_truthy(s)) {
            output += &format!("- **Summary**: {}\n", py_str(summary));
        }
        output += "\n";
    }
    output
}

/// The node's `name` (its id when it has none) and `type` (`unknown`).
fn name_and_type(view: &GraphView, id: &str) -> (String, String) {
    let node = view.node(id);
    (
        node.and_then(|n| n.get("name"))
            .map_or_else(|| id.to_owned(), py_str),
        node.and_then(|n| n.get("type"))
            .map_or_else(|| "unknown".to_owned(), py_str),
    )
}

/// The chat agent's `get_community_detail` tool.
#[must_use]
pub fn get_community_detail(view: &GraphView, community_id: &str) -> String {
    let all = communities(view);
    let Some(community) = all
        .and_then(|all| all.get(community_id))
        .filter(|c| py_truthy(c))
    else {
        let available: Vec<&str> = all
            .map(|all| all.keys().take(10).map(String::as_str).collect())
            .unwrap_or_default();
        let available = if available.is_empty() {
            "none".to_owned()
        } else {
            available.join(", ")
        };
        return format!("Community '{community_id}' not found. Available: {available}");
    };
    let empty = Map::new();
    let community = community.as_object().unwrap_or(&empty);
    let members = items(community.get("members"));
    let centroids = items(community.get("centroids"));
    let stats = community
        .get("stats")
        .and_then(Value::as_object)
        .unwrap_or(&empty);
    let label = text_or(community, "label", community_id);
    let zero = Value::from(0);
    let mut output = format!("# {community_id}: {label}\n\n## Statistics\n");
    output += &format!(
        "- Size: {} entities\n",
        stats
            .get("size")
            .map_or_else(|| members.len().to_string(), py_str)
    );
    for (title, key) in [("Density", "density"), ("Cohesion", "cohesion")] {
        let value = stats.get(key).unwrap_or(&zero);
        let Some(shown) = fixed(value, 4) else {
            return format!("Error getting community detail: {}", format_error(value));
        };
        output += &format!("- {title}: {shown}\n");
    }
    output += &format!(
        "- Internal edges: {}\n\n",
        text_or(stats, "internal_edges", "0")
    );
    if let Some(summary) = community.get("summary").filter(|s| py_truthy(s)) {
        output += &format!("## Summary\n{}\n\n", py_str(summary));
    }
    output += "## Key Entities (Centroids)\n";
    let mut centroid_ids = HashSet::new();
    for centroid in centroids {
        let field = |key: &str| centroid.get(key).map_or_else(String::new, py_str);
        let id = field("id");
        let score = centroid.get("score").unwrap_or(&Value::Null);
        let Some(score) = fixed(score, 3) else {
            return format!("Error getting community detail: {}", format_error(score));
        };
        output += &format!("- **{}** ({}, score={score})", field("name"), field("type"));
        if let Some(signature) = view
            .node(&id)
            .and_then(|node| node.get("signature"))
            .filter(|s| py_truthy(s))
        {
            output += &format!(": `{}`", py_str(signature));
        }
        output += "\n";
        centroid_ids.insert(id);
    }
    output += &members_section(view, members, &centroid_ids);
    if let Some(micro) = community
        .get("micro_clusters")
        .filter(|m| py_truthy(m))
        .and_then(Value::as_object)
    {
        output += &format!("\n## Micro-clusters ({})\n", micro.len());
        for (id, info) in micro {
            let info = info.as_object().unwrap_or(&empty);
            output += &format!(
                "- **{id}**: {} ({} entities)\n",
                text_or(info, "label", id),
                items(info.get("members")).len()
            );
        }
    }
    output
}

/// The members of a community that are not centroids: the architectural
/// ones by type priority then name (at most 30), and a count of the rest.
fn members_section(view: &GraphView, members: &[Value], centroid_ids: &HashSet<String>) -> String {
    let mut output = format!("\n## Members ({} total)\n", members.len());
    let mut others: Vec<(i64, String, String, String)> = members
        .iter()
        .map(py_str)
        .filter(|id| !centroid_ids.contains(id))
        .map(|id| {
            let (name, kind) = name_and_type(view, &id);
            (priority(&kind), name.to_lowercase(), name, kind)
        })
        .collect();
    others.sort_by(|a, b| b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
    let minimum = tables().architectural_min_priority;
    let architectural: Vec<&(i64, String, String, String)> =
        others.iter().filter(|m| m.0 >= minimum).collect();
    let noise = others.len() - architectural.len();
    for (_, _, name, kind) in architectural.iter().take(30) {
        output += &format!("- {name} ({kind})\n");
    }
    if architectural.len() > 30 {
        output += &format!(
            "- ... and {} more architectural members\n",
            architectural.len() - 30
        );
    }
    if noise > 0 {
        output += &format!(
            "- ({noise} implementation-level members: methods, variables, imports, etc.)\n"
        );
    }
    output
}

/// The node's `community_id`, when it has one.
fn community_of(view: &GraphView, id: &str) -> Option<String> {
    view.node(id)
        .filter(|node| !node.is_empty())
        .and_then(|node| node.get("community_id"))
        .filter(|c| py_truthy(c))
        .map(py_str)
}

/// The chat agent's `find_entity_community` tool: the five best lexical
/// matches for `entity_name`, each with its community.
#[must_use]
pub fn find_entity_community(view: &GraphView, entity_name: &str) -> String {
    let hits = lexical::search(view, entity_name, 5);
    if hits.is_empty() {
        return format!("Entity '{entity_name}' not found in the graph.");
    }
    let mut output = format!("# Community membership for '{entity_name}'\n\n");
    for hit in hits {
        let (name, kind) = name_and_type(view, &hit.id);
        match community_of(view, &hit.id) {
            Some(community) => {
                let label = communities(view)
                    .and_then(|all| all.get(&community))
                    .and_then(Value::as_object)
                    .filter(|c| !c.is_empty())
                    .map_or_else(|| community.clone(), |c| text_or(c, "label", &community));
                output += &format!("- **{name}** ({kind}) → **{community}**: {label}\n");
            }
            None => output += &format!("- **{name}** ({kind}) → no community assigned\n"),
        }
    }
    output
}

/// The chat agent's `search_within_community` tool: the lexical matches
/// (of the best 200) that are members, architectural types first.
#[must_use]
pub fn search_within_community(view: &GraphView, community_id: &str, query: &str) -> String {
    let members: Vec<String> = communities(view)
        .and_then(|all| all.get(community_id))
        .map(|info| items(info.get("members")).iter().map(py_str).collect())
        .unwrap_or_default();
    if members.is_empty() {
        return format!("Community '{community_id}' not found or has no members.");
    }
    let member_set: HashSet<&str> = members.iter().map(String::as_str).collect();
    let mut found: Vec<(i64, lexical::Hit)> = lexical::search(view, query, 200)
        .into_iter()
        .filter(|hit| member_set.contains(hit.id.as_str()))
        .map(|hit| {
            let (_, kind) = name_and_type(view, &hit.id);
            (priority(&kind), hit)
        })
        .collect();
    if found.is_empty() {
        return format!(
            "No entities matching '{query}' found in {community_id} ({} members).",
            members.len()
        );
    }
    found.sort_by(|(a_priority, a), (b_priority, b)| {
        b_priority
            .cmp(a_priority)
            .then_with(|| b.score.total_cmp(&a.score))
    });
    let mut output = format!("# Search results in {community_id} for '{query}'\n\n");
    let empty = Map::new();
    for (index, (_, hit)) in found.iter().take(20).enumerate() {
        let node = view.node(&hit.id).unwrap_or(&empty);
        let name = text_or(node, "name", "");
        let kind = text_or(node, "type", "unknown");
        let shown = match node.get("layer").filter(|layer| py_truthy(layer)) {
            Some(layer) => format!("{}/{kind}", py_str(layer)),
            None => kind,
        };
        output += &format!("{:2}. **{name}** ({shown})\n", index + 1);
        output += &location_and_description(node, false);
        output += "\n";
    }
    if found.len() > 20 {
        output += &format!("\n_Showing 20 of {} matches._\n", found.len());
    }
    output
}

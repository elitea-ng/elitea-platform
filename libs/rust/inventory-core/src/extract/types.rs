//! Entity type normalisation: the Python pipeline's two passes.
//!
//! An LLM entity's type goes through `normalize_entity_type` (ingestion)
//! and is then normalised again by `KnowledgeGraph.add_entity`
//! (`_normalize_entity_type`, which every entity passes through, parser
//! symbols included). The id is computed from the first pass's type; the
//! node stores the second's. Both functions run on the Python engine's own
//! tables (`assets`), and `normalize_goldens` holds what the Python
//! functions return for a set of messy types.

use super::assets::assets;

/// `normalize_entity_type` in `ingestion.py`.
#[must_use]
pub fn normalize_ingestion(entity_type: &str) -> String {
    if entity_type.is_empty() {
        return "unknown".to_owned();
    }
    let tables = assets();
    if let Some(mapped) = tables.type_normalization_map.get(entity_type) {
        return mapped.clone();
    }
    let normalized =
        elitea_engine_core::pystr::strip(&entity_type.to_lowercase()).replace([' ', '-'], "_");
    let canonical = |name: &str| tables.canonical_types.iter().any(|t| t == name);
    if canonical(&normalized) {
        return normalized;
    }
    if normalized.ends_with('s') && !normalized.ends_with("ss") && normalized.chars().count() > 3 {
        let singular = &normalized[..normalized.len() - 1];
        if canonical(singular) {
            return singular.to_owned();
        }
    }
    normalized
}

/// `_insert_word_boundaries`.
fn insert_word_boundaries(type_str: &str) -> String {
    if type_str.contains('_') {
        return type_str.to_owned();
    }
    let tables = assets();
    for prefix in &tables.known_type_prefixes {
        if let Some(suffix) = type_str.strip_prefix(prefix.as_str())
            && !suffix.is_empty()
            && !suffix.starts_with('_')
        {
            let valid = tables
                .known_type_suffixes
                .iter()
                .any(|s| suffix.starts_with(s.as_str()))
                || tables.type_normalization_map.contains_key(suffix)
                || tables.type_priority.contains_key(suffix);
            if valid {
                return format!("{prefix}_{suffix}");
            }
        }
    }
    type_str.to_owned()
}

/// `p.lower().strip().strip("_")`.
fn clean_part(part: &str) -> String {
    elitea_engine_core::pystr::strip(&part.to_lowercase())
        .trim_matches('_')
        .to_owned()
}

/// The singular when `p` is a plural of a known type.
fn known_singular(p: String) -> String {
    let tables = assets();
    if p.ends_with('s') && !p.ends_with("ss") && p.chars().count() > 3 {
        let singular = &p[..p.len() - 1];
        if tables.type_normalization_map.contains_key(singular)
            || tables.type_priority.contains_key(singular)
        {
            return singular.to_owned();
        }
    }
    p
}

/// `_pick_best_type_part`: the part of highest `TYPE_PRIORITY` (the first
/// of equals), normalised.
fn pick_best_type_part(parts: &[String]) -> String {
    let tables = assets();
    match parts {
        [] => return "unknown".to_owned(),
        [only] => return only.clone(),
        _ => {}
    }
    let priority = |part: &str| {
        let p = known_singular(insert_word_boundaries(&clean_part(part)));
        let mapped = tables.type_normalization_map.get(&p).cloned().unwrap_or(p);
        tables.type_priority.get(&mapped).copied().unwrap_or(0)
    };
    let mut best = &parts[0];
    let mut best_priority = priority(best);
    for part in &parts[1..] {
        let candidate = priority(part);
        if candidate > best_priority {
            best = part;
            best_priority = candidate;
        }
    }
    known_singular(insert_word_boundaries(&clean_part(best)))
}

/// `_normalize_entity_type` in `knowledge_graph.py`.
#[must_use]
pub fn normalize_graph(entity_type: &str) -> String {
    let tables = assets();
    let map = &tables.type_normalization_map;
    if entity_type.is_empty() {
        return "unknown".to_owned();
    }
    if let Some(mapped) = map.get(entity_type) {
        return mapped.clone();
    }
    let mut normalized = elitea_engine_core::pystr::strip(&entity_type.to_lowercase())
        .replace([' ', '-'], "_")
        .replace("_/", "/")
        .replace("___", "/")
        .replace("::", "/");
    if normalized.contains(':') && !normalized.contains('/') {
        normalized = normalized.replace(':', "/");
    }
    while normalized.contains("__") {
        normalized = normalized.replace("__", "_");
    }
    normalized = normalized.trim_matches('_').to_owned();
    if let Some(mapped) = map.get(&normalized) {
        return mapped.clone();
    }
    if normalized.contains('(') && normalized.ends_with(')') {
        let mut halves = normalized.split('(');
        let base = halves
            .next()
            .unwrap_or_default()
            .trim_matches('_')
            .to_owned();
        let inner = halves
            .next()
            .unwrap_or_default()
            .trim_end_matches(')')
            .trim_matches('_')
            .to_owned();
        if !base.is_empty() && !inner.is_empty() {
            normalized = pick_best_type_part(&[base, inner]);
        } else if !base.is_empty() {
            normalized = base;
        }
    }
    if normalized.contains("_or_") {
        let parts: Vec<String> = normalized.split("_or_").map(str::to_owned).collect();
        normalized = pick_best_type_part(&parts);
    }
    if normalized.contains(',') {
        let parts: Vec<String> = normalized
            .split(',')
            .map(|p| {
                elitea_engine_core::pystr::strip(p)
                    .trim_matches('_')
                    .to_owned()
            })
            .filter(|p| !p.is_empty())
            .collect();
        if !parts.is_empty() {
            normalized = pick_best_type_part(&parts);
        }
    }
    if normalized.contains('/') {
        let parts: Vec<String> = normalized
            .split('/')
            .map(|p| {
                elitea_engine_core::pystr::strip(p)
                    .trim_matches('_')
                    .to_owned()
            })
            .filter(|p| !p.is_empty())
            .collect();
        if parts.is_empty() {
            return "unknown".to_owned();
        }
        normalized = pick_best_type_part(&parts);
    }
    normalized = insert_word_boundaries(&normalized);
    if let Some(mapped) = map.get(&normalized) {
        return mapped.clone();
    }
    if normalized.ends_with('s') && !normalized.ends_with("ss") && normalized.chars().count() > 3 {
        let singular = &normalized[..normalized.len() - 1];
        if let Some(mapped) = map.get(singular) {
            return mapped.clone();
        }
        if map.values().any(|value| value == singular) {
            return singular.to_owned();
        }
    }
    let mut suffixes: Vec<&String> = tables.type_suffix_normalization.keys().collect();
    // `sorted(..., key=len, reverse=True)`: stable, so equal lengths keep
    // the table's order.
    suffixes.sort_by_key(|suffix| std::cmp::Reverse(suffix.chars().count()));
    for suffix in suffixes {
        if normalized.ends_with(suffix.as_str()) && normalized.len() > suffix.len() {
            let prefix = &normalized[..normalized.len() - suffix.len()];
            if !prefix.is_empty() && prefix != "_" {
                return tables.type_suffix_normalization[suffix].clone();
            }
        }
    }
    normalized
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    #[test]
    fn both_passes_return_what_the_python_functions_return() {
        let asset: Value = serde_json::from_str(include_str!("../../assets/python_inventory.json"))
            .unwrap_or_default();
        let goldens = asset["normalize_goldens"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        assert!(goldens.len() > 40);
        for golden in goldens {
            let raw = golden["raw"].as_str().unwrap_or_default();
            let first = normalize_ingestion(raw);
            assert_eq!(
                Some(first.as_str()),
                golden["ingestion"].as_str(),
                "ingestion pass of {raw:?}"
            );
            assert_eq!(
                Some(normalize_graph(&first).as_str()),
                golden["graph"].as_str(),
                "graph pass of {raw:?}"
            );
        }
    }
}

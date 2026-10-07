//! Pass 3: tiered lexical resolution T1–T4 behind an IDF gate
//! (`graph_lexical_v2.resolve_orphans_lexical_tiered`).
//!
//! One lexical search per orphan (at most 16 hits), partitioned in memory:
//!
//! * T1 — same directory, architectural target;
//! * T2 — parent directory (any target);
//! * T3 — anywhere, architectural target;
//! * T4 — anywhere, only for names of 8+ characters with a high IDF.
//!
//! The first non-empty tier wins (two hits at most). Which tiers a node
//! may use depends on its `symbol_type` and on the name's IDF,
//! `ln(nodes / (1 + phrase matches))`: at least 3.0 opens every allowed
//! tier, at least 1.5 only T1/T2, below nothing.
//!
//! A generic class name (`API`, `Resource`, …) on a REST surface is
//! searched as `"<file stem> <name>"` and the GRAPH node is re-typed
//! `rest_endpoint` (the index row is not), which then makes it eligible for
//! every tier.

use super::hybrid::{FTS_MIN_SCORE_NORM, orphan_rel_path, orphan_symbol_name};
use super::pynum::count_f64;
use super::{Ctx, SearchHit, StoreError};
use crate::graph::pystr;
use crate::graph::{Label, NodeData};
use serde_json::Value;

/// `ARCH_TARGETS`: the target types T1 and T3 accept.
pub const ARCH_TARGETS: &[&str] = &[
    "module",
    "class",
    "interface",
    "function",
    "method",
    "constant",
    "module_doc",
    "file_doc",
    "rest_endpoint",
];

/// `GENERIC_CLASS_NAMES`.
pub const GENERIC_CLASS_NAMES: &[&str] = &[
    "API",
    "Resource",
    "View",
    "ViewSet",
    "Endpoint",
    "Handler",
    "Controller",
    "Service",
    "Manager",
    "Mixin",
    "Base",
    "Abstract",
    "Interface",
];

/// `REST_DECORATORS` (matched as prefixes).
pub const REST_DECORATORS: &[&str] = &[
    "@app.route",
    "@router.get",
    "@router.post",
    "@router.put",
    "@router.delete",
    "@router.patch",
    "@api_view",
    "@route",
    "@get",
    "@post",
    "@put",
    "@delete",
    "@patch",
    "@RestController",
    "@RequestMapping",
    "@GetMapping",
    "@PostMapping",
    "@PutMapping",
    "@DeleteMapping",
    "@PatchMapping",
    "@expose",
];

/// `REST_BASE_CLASSES`.
pub const REST_BASE_CLASSES: &[&str] = &[
    "APIView",
    "Resource",
    "ViewSet",
    "HTTPEndpoint",
    "MethodView",
    "Controller",
    "RouteHandler",
    "GenericAPIView",
];

/// `_IDF_HIGH`.
pub const IDF_HIGH: f64 = 3.0;
/// `_IDF_LOW`.
pub const IDF_LOW: f64 = 1.5;
/// `_T4_MIN_NAME_LEN`.
pub const T4_MIN_NAME_LEN: usize = 8;
/// `max(fts_limit, 16)` with the cascade's `fts_limit` of 8.
pub const TIERED_SEARCH_LIMIT: usize = 16;

/// A set of tiers.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Tiers(u8);

impl Tiers {
    pub const T1: Self = Self(1);
    pub const T2: Self = Self(2);
    pub const T3: Self = Self(4);
    pub const T4: Self = Self(8);
    const ALL: Self = Self(15);
    const T1_T2: Self = Self(3);
    const T1_T3: Self = Self(7);

    #[must_use]
    pub fn contains(self, tier: Self) -> bool {
        self.0 & tier.0 == tier.0
    }

    #[must_use]
    pub fn is_empty(self) -> bool {
        self.0 == 0
    }

    #[must_use]
    fn intersect(self, other: Self) -> Self {
        Self(self.0 & other.0)
    }
}

/// `_TIER_ELIGIBILITY`.
fn eligibility(symbol_type: &str) -> Tiers {
    match symbol_type {
        "module" | "class" | "interface" | "struct" | "enum" | "trait" | "rest_endpoint" => {
            Tiers::ALL
        }
        "function" => Tiers::T1_T3,
        "method" => Tiers::T1_T2,
        _ => Tiers::default(),
    }
}

/// `_allowed_tiers`: eligibility intersected with the IDF gate.
#[must_use]
pub fn allowed_tiers(symbol_type: &str, idf: f64) -> Tiers {
    let base = eligibility(symbol_type);
    if base.is_empty() {
        return base;
    }
    if idf >= IDF_HIGH {
        base
    } else if idf >= IDF_LOW {
        base.intersect(Tiers::T1_T2)
    } else {
        Tiers::default()
    }
}

/// `_detect_rest_endpoint` over the node's attributes (`decorators`,
/// `base_classes` / `bases`) and its source text (the index row's when
/// there is one: Python merged `node_data | db_node`).
#[must_use]
pub fn detect_rest_endpoint(node: Option<&NodeData>, source_text: &str) -> bool {
    let strings = |key: &str| -> Vec<&str> {
        match node.and_then(|n| n.extra.get(key)) {
            Some(Value::Array(items)) => items.iter().filter_map(Value::as_str).collect(),
            _ => Vec::new(),
        }
    };
    for decorator in strings("decorators") {
        let head = pystr::strip(decorator);
        if head.is_empty() {
            continue;
        }
        let head = if head.starts_with('@') {
            head.to_owned()
        } else {
            format!("@{head}")
        };
        if REST_DECORATORS.iter().any(|p| head.starts_with(p)) {
            return true;
        }
    }
    // `node_data.get("base_classes") or node_data.get("bases")`.
    let mut bases = strings("base_classes");
    if bases.is_empty() {
        bases = strings("bases");
    }
    if bases
        .iter()
        .any(|b| REST_BASE_CLASSES.contains(&b.rsplit('.').next().unwrap_or("")))
    {
        return true;
    }
    for line in pystr::splitlines(source_text).into_iter().take(20) {
        let stripped = pystr::strip(line);
        if stripped.is_empty() || stripped.starts_with('#') || !stripped.starts_with('@') {
            continue;
        }
        if REST_DECORATORS.iter().any(|p| stripped.starts_with(p)) {
            return true;
        }
    }
    false
}

/// `_file_stem`: `"users"` for `"src/api/users.py"`.
#[must_use]
pub fn file_stem(rel_path: &str) -> &str {
    let base = rel_path.rsplit('/').next().unwrap_or("");
    base.rsplit_once('.').map_or(base, |(stem, _)| stem)
}

/// `_compute_fts_symbol_name`: `"users API"` for `("API", "users")`.
#[must_use]
pub fn disambiguated_query(symbol_name: &str, stem: &str) -> String {
    if stem.is_empty() || stem.to_lowercase() == symbol_name.to_lowercase() {
        symbol_name.to_owned()
    } else {
        format!("{stem} {symbol_name}")
    }
}

/// `_node_dir`.
fn node_dir(rel_path: &str) -> &str {
    rel_path.rsplit_once('/').map_or("", |(dir, _)| dir)
}

/// `_parent_dir`.
fn parent_dir(rel_path: &str) -> &str {
    node_dir(node_dir(rel_path))
}

/// `_filter_by_dir`: hits in `directory` or below.
fn in_dir<'h>(hits: &'h [SearchHit], directory: &str) -> Vec<&'h SearchHit> {
    if directory.is_empty() {
        return Vec::new();
    }
    hits.iter()
        .filter(|h| {
            !h.rel_path.is_empty()
                && (h.rel_path == directory
                    || h.rel_path
                        .strip_prefix(directory)
                        .is_some_and(|rest| rest.starts_with('/')))
        })
        .collect()
}

fn is_arch_target(hit: &SearchHit) -> bool {
    ARCH_TARGETS.contains(&hit.symbol_type.as_str())
}

/// A chosen hit and its tier (`dict(h, _tier=…)`).
#[derive(Debug, Clone, PartialEq)]
pub struct TieredHit {
    pub hit: SearchHit,
    pub tier: &'static str,
}

fn take(hits: Vec<&SearchHit>, tier: &'static str) -> Vec<TieredHit> {
    hits.into_iter()
        .take(super::cascade::MAX_LEXICAL_EDGES)
        .map(|hit| TieredHit {
            hit: hit.clone(),
            tier,
        })
        .collect()
}

/// `resolve_orphans_lexical_tiered` for one orphan.
///
/// # Errors
///
/// The store's.
pub fn resolve_orphan_tiered(ctx: &mut Ctx<'_>, id: &str) -> Result<Vec<TieredHit>, StoreError> {
    let symbol_name = orphan_symbol_name(ctx, id).to_owned();
    if symbol_name.chars().count() < 2 {
        return Ok(Vec::new());
    }
    let rel_path = orphan_rel_path(ctx, id).to_owned();
    let mut symbol_type = match ctx.row(id) {
        Some(row) if !row.symbol_type.is_empty() => row.symbol_type.clone(),
        _ => ctx
            .graph
            .node(id)
            .map(|n| n.symbol_type.to_string())
            .unwrap_or_default(),
    };

    // REST disambiguation (`orphan_rest_disambig`, on).
    let mut query = symbol_name.clone();
    if GENERIC_CLASS_NAMES.contains(&symbol_name.as_str()) {
        let source_text = match ctx.row(id) {
            Some(row) => row.source_text.clone().unwrap_or_default(),
            None => ctx
                .graph
                .node(id)
                .map(|n| n.source_text.clone())
                .unwrap_or_default(),
        };
        if detect_rest_endpoint(ctx.graph.node(id), &source_text) {
            query = disambiguated_query(&symbol_name, file_stem(&rel_path));
            if let Some(node) = ctx.graph.node_mut(id) {
                let previous =
                    std::mem::replace(&mut node.symbol_type, Label::Borrowed("rest_endpoint"));
                ctx.note_retyped(id, previous);
            }
            "rest_endpoint".clone_into(&mut symbol_type);
        }
    }

    // IDF gate (`orphan_lexical_idf_gate`, on).
    let total = ctx.store.node_count()?;
    let idf = if total == 0 {
        f64::INFINITY
    } else {
        let matches = ctx.store.count_phrase_matches(&query)?;
        let as_count = |n: u64| count_f64(usize::try_from(n).unwrap_or(usize::MAX));
        (as_count(total) / as_count(1 + matches)).ln()
    };
    let allowed = allowed_tiers(&symbol_type, idf);
    if allowed.is_empty() {
        return Ok(Vec::new());
    }

    let mut hits = ctx
        .store
        .search_lexical(&query, None, TIERED_SEARCH_LIMIT)?;
    hits.retain(|hit| {
        hit.node_id != id && hit.score_norm.is_none_or(|norm| norm >= FTS_MIN_SCORE_NORM)
    });
    if hits.is_empty() {
        return Ok(Vec::new());
    }
    let own = node_dir(&rel_path);
    let parent = parent_dir(&rel_path);

    if allowed.contains(Tiers::T1) && !own.is_empty() {
        let t1: Vec<&SearchHit> = in_dir(&hits, own)
            .into_iter()
            .filter(|h| is_arch_target(h))
            .collect();
        if !t1.is_empty() {
            return Ok(take(t1, "T1"));
        }
    }
    if allowed.contains(Tiers::T2) && !parent.is_empty() {
        let t2 = in_dir(&hits, parent);
        if !t2.is_empty() {
            return Ok(take(t2, "T2"));
        }
    }
    if allowed.contains(Tiers::T3) {
        let t3: Vec<&SearchHit> = hits.iter().filter(|h| is_arch_target(h)).collect();
        if !t3.is_empty() {
            return Ok(take(t3, "T3"));
        }
    }
    if allowed.contains(Tiers::T4)
        && symbol_name.chars().count() >= T4_MIN_NAME_LEN
        && idf >= IDF_HIGH
    {
        return Ok(take(hits.iter().collect(), "T4"));
    }
    Ok(Vec::new())
}

#[cfg(test)]
#[allow(clippy::float_cmp)] // bit-exact parity is the point
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_idf_gate_narrows_the_tiers() {
        assert_eq!(allowed_tiers("class", 3.0), Tiers::ALL);
        assert_eq!(allowed_tiers("class", 2.0), Tiers::T1_T2);
        assert!(allowed_tiers("class", 1.0).is_empty());
        assert_eq!(allowed_tiers("function", 9.0), Tiers::T1_T3);
        assert!(!allowed_tiers("function", 9.0).contains(Tiers::T4));
        assert!(allowed_tiers("parameter", 9.0).is_empty());
        // NaN compares false everywhere: no tier, as in Python.
        assert!(allowed_tiers("class", f64::NAN).is_empty());
    }

    #[test]
    fn rest_endpoints_are_detected_by_decorator_base_or_source() {
        let mut node = NodeData::default();
        assert!(!detect_rest_endpoint(Some(&node), "class API:\n    pass"));
        assert!(detect_rest_endpoint(
            Some(&node),
            "# routes\n@router.get('/x')\nclass API: ..."
        ));
        node.extra.insert("decorators", json!(["app.route('/')"]));
        assert!(detect_rest_endpoint(Some(&node), ""));
        let mut node = NodeData::default();
        node.extra
            .insert("bases", json!(["rest_framework.APIView"]));
        assert!(detect_rest_endpoint(Some(&node), ""));
    }

    #[test]
    fn generic_names_get_the_file_stem() {
        assert_eq!(file_stem("src/api/users.py"), "users");
        assert_eq!(file_stem("Makefile"), "Makefile");
        assert_eq!(disambiguated_query("API", "users"), "users API");
        assert_eq!(disambiguated_query("API", "api"), "API");
        assert_eq!(disambiguated_query("API", ""), "API");
    }

    #[test]
    fn directories_match_whole_segments() {
        let hit = |p: &str| SearchHit {
            rel_path: p.to_owned(),
            ..SearchHit::default()
        };
        let hits = [hit("src/a/x.py"), hit("src/ab/y.py"), hit("src/a"), hit("")];
        let found: Vec<&str> = in_dir(&hits, "src/a")
            .iter()
            .map(|h| h.rel_path.as_str())
            .collect();
        assert_eq!(found, ["src/a/x.py", "src/a"]);
        assert_eq!(parent_dir("a/b/c.py"), "a");
        assert_eq!(parent_dir("b/c.py"), "");
    }
}

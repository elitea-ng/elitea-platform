//! The Python engine's prompts and type tables (`assets/python_inventory.json`,
//! generated from its source by `assets/generate.py`).

use indexmap::IndexMap;
use serde::Deserialize;
use std::sync::OnceLock;

const ASSET: &str = include_str!("../../assets/python_inventory.json");

/// The four extraction prompts, as `LangChain` f-string templates.
#[derive(Debug, Deserialize)]
pub struct Prompts {
    pub entity: String,
    pub relation: String,
    pub fact: String,
    pub code_fact: String,
}

/// Everything the asset holds that the engine reads.
#[derive(Debug, Deserialize)]
pub struct Assets {
    pub prompts: Prompts,
    pub canonical_types: Vec<String>,
    pub fact_types: Vec<String>,
    pub code_fact_types: Vec<String>,
    pub type_normalization_map: IndexMap<String, String>,
    pub type_priority: IndexMap<String, i64>,
    pub type_suffix_normalization: IndexMap<String, String>,
    pub known_type_prefixes: Vec<String>,
    pub known_type_suffixes: Vec<String>,
    pub significant_entity_types: Vec<String>,
}

/// The parsed asset. It is compiled in, so a parse failure is a build
/// defect a test catches; the engine never runs with a broken one.
///
/// # Panics
///
/// Never on a tested build (`the_asset_parses`).
#[must_use]
pub fn assets() -> &'static Assets {
    static PARSED: OnceLock<Assets> = OnceLock::new();
    #[allow(clippy::expect_used)]
    PARSED.get_or_init(|| serde_json::from_str(ASSET).expect("assets/python_inventory.json parses"))
}

/// Render a `LangChain` f-string template: `{name}` is replaced by its value,
/// `{{` and `}}` are literal braces.
#[must_use]
pub fn render(template: &str, values: &[(&str, &str)]) -> String {
    let mut out = String::with_capacity(template.len());
    let mut rest = template;
    while let Some(index) = rest.find(['{', '}']) {
        out.push_str(&rest[..index]);
        let tail = &rest[index..];
        if let Some(after) = tail.strip_prefix("{{") {
            out.push('{');
            rest = after;
        } else if let Some(after) = tail.strip_prefix("}}") {
            out.push('}');
            rest = after;
        } else if let Some(close) = tail.find('}').filter(|_| tail.starts_with('{')) {
            let name = &tail[1..close];
            match values.iter().find(|(key, _)| *key == name) {
                Some((_, value)) => out.push_str(value),
                None => out.push_str(&tail[..=close]),
            }
            rest = &tail[close + 1..];
        } else {
            out.push_str(&tail[..1]);
            rest = &tail[1..];
        }
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_asset_parses() {
        let parsed = assets();
        assert!(parsed.type_normalization_map.len() > 100);
        assert!(parsed.prompts.entity.contains("{content}"));
        assert_eq!(parsed.fact_types.len(), 6);
    }

    #[test]
    fn templates_render_like_langchain() {
        assert_eq!(
            render(
                "a {x} {{\"k\": {y}}} {missing}",
                &[("x", "1"), ("y", "{z}")]
            ),
            "a 1 {\"k\": {z}} {missing}"
        );
        let rendered = render(
            &assets().prompts.relation,
            &[
                ("content", "C"),
                ("entities_list", "L"),
                ("schema_section", ""),
            ],
        );
        assert!(
            rendered.contains("{\"source_id\": \"407b9c0c2048\""),
            "doubled braces are literal"
        );
        assert!(!rendered.contains("{content}"));
    }
}

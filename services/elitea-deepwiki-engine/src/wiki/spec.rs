//! The wiki structure and page models (`state/wiki_state.py`).
//!
//! A faithful port of `PageSpec`, `SectionSpec`, `WikiStructureSpec` and
//! `WikiPage`: the field names and ORDER are `model_dump()`'s, because the
//! structure travels between the planner and the page path as JSON and the
//! parity gate compares it as text.
//!
//! MERGE NOTE: the structure planner is ported in parallel (another
//! branch) and carries its own copy of these models. Both copies port the
//! same Python shape; the merge keeps one of them.

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

/// `PageSpec`: one page the planner asked for.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct PageSpec {
    pub page_name: String,
    pub page_order: i64,
    pub description: String,
    pub content_focus: String,
    /// Why the page exists. The cluster planner writes "Grouped by graph
    /// clustering (macro=…)", which is what routes a page to cluster
    /// expansion.
    pub rationale: String,
    #[serde(default)]
    pub target_symbols: Vec<String>,
    #[serde(default)]
    pub target_docs: Vec<String>,
    #[serde(default)]
    pub target_folders: Vec<String>,
    #[serde(default)]
    pub key_files: Vec<String>,
    #[serde(default)]
    pub retrieval_query: String,
    /// `section_id` / `page_id` (the macro / micro cluster) and
    /// `cluster_node_ids` from the cluster planner.
    #[serde(default)]
    pub metadata: Map<String, Value>,
}

/// `SectionSpec`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SectionSpec {
    pub section_name: String,
    pub section_order: i64,
    pub description: String,
    pub rationale: String,
    pub pages: Vec<PageSpec>,
}

/// `WikiStructureSpec`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WikiStructureSpec {
    pub wiki_title: String,
    pub overview: String,
    pub sections: Vec<SectionSpec>,
    pub total_pages: i64,
}

/// `WikiPage.status` values the live path sets.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PageStatus {
    Completed,
    Failed,
}

impl PageStatus {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Failed => "failed",
        }
    }
}

/// `WikiPage`: one generated page. `page_id` is `"{section}#{page}"`, the
/// indices in the (split) structure.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WikiPage {
    pub page_id: String,
    pub title: String,
    /// Empty for a failed page (exported as an empty file, as Python did).
    pub content: String,
    pub status: PageStatus,
}

impl WikiStructureSpec {
    /// The structure as `model_dump()` gives it.
    ///
    /// # Errors
    ///
    /// Never in practice: every field is plain JSON.
    pub fn to_value(&self) -> Result<Value, serde_json::Error> {
        serde_json::to_value(self)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn model_dump_order_is_python_field_order() {
        let page = PageSpec {
            page_name: "P".into(),
            page_order: 1,
            description: "d".into(),
            content_focus: "c".into(),
            rationale: "r".into(),
            target_symbols: vec!["A".into()],
            target_docs: Vec::new(),
            target_folders: Vec::new(),
            key_files: Vec::new(),
            retrieval_query: String::new(),
            metadata: Map::new(),
        };
        let text = crate::pyjson::dumps(&serde_json::to_value(&page).unwrap_or(Value::Null));
        assert_eq!(
            text,
            "{\"page_name\": \"P\", \"page_order\": 1, \"description\": \"d\", \"content_focus\": \"c\", \"rationale\": \"r\", \"target_symbols\": [\"A\"], \"target_docs\": [], \"target_folders\": [], \"key_files\": [], \"retrieval_query\": \"\", \"metadata\": {}}"
        );
    }

    #[test]
    fn defaults_fill_missing_lists() {
        let spec: Result<PageSpec, _> = serde_json::from_str(
            r#"{"page_name":"P","page_order":1,"description":"d","content_focus":"c","rationale":"r"}"#,
        );
        assert!(spec.is_ok_and(|s| s.target_symbols.is_empty() && s.metadata.is_empty()));
    }
}

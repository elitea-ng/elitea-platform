//! The SDK wrapper's folder-tree helpers (`_build_folder_hierarchy`,
//! `_find_folders_by_name`, `_get_folder_paths`, `_find_folder_by_path`,
//! `_collect_subfolders`) and its `_parse_tests` projection.

use std::collections::{HashMap, HashSet};

use serde_json::Value;

use super::render::py_str_opt;

/// Folders by id in first-seen order, with each folder's children.
pub(in crate::toolkits) struct FolderTree {
    order: Vec<i64>,
    nodes: HashMap<i64, Node>,
}

struct Node {
    folder: Option<Value>,
    children: Vec<i64>,
}

fn folder_id(value: Option<&Value>) -> Option<i64> {
    match value? {
        Value::Number(number) => number.as_i64(),
        Value::String(text) => text.parse().ok(),
        _ => None,
    }
}

/// Python truthiness of a `parentId`: absent, null, 0 and "" are no parent.
fn parent_of(folder: &Value) -> Option<i64> {
    folder_id(folder.get("parentId")).filter(|id| *id != 0)
}

impl FolderTree {
    /// `_build_folder_hierarchy`.
    #[must_use]
    pub(in crate::toolkits) fn build(folders: &[Value]) -> Self {
        let mut tree = Self {
            order: Vec::new(),
            nodes: HashMap::new(),
        };
        for folder in folders {
            let Some(id) = folder_id(folder.get("id")) else {
                continue;
            };
            tree.node(id).folder = Some(folder.clone());
            if let Some(parent) = parent_of(folder) {
                tree.node(parent).children.push(id);
            }
        }
        tree
    }

    fn node(&mut self, id: i64) -> &mut Node {
        if !self.nodes.contains_key(&id) {
            self.order.push(id);
        }
        self.nodes.entry(id).or_insert_with(|| Node {
            folder: None,
            children: Vec::new(),
        })
    }

    /// `_get_folder_paths` then `_find_folder_by_path`: the first folder, in
    /// depth-first order from the roots, whose `/`-joined name path equals
    /// `path`.
    #[must_use]
    pub(in crate::toolkits) fn find_by_path(&self, path: &str) -> Option<i64> {
        let roots = self.order.iter().copied().filter(|id| {
            self.nodes[id]
                .folder
                .as_ref()
                .is_some_and(|folder| parent_of(folder).is_none())
        });
        let mut paths = Vec::new();
        let mut visited = HashSet::new();
        for root in roots {
            self.paths_from(root, "", &mut paths, &mut visited);
        }
        paths
            .into_iter()
            .find(|(_, candidate)| candidate == path)
            .map(|(id, _)| id)
    }

    fn paths_from(
        &self,
        id: i64,
        prefix: &str,
        paths: &mut Vec<(i64, String)>,
        visited: &mut HashSet<i64>,
    ) {
        let Some(node) = self.nodes.get(&id) else {
            return;
        };
        let Some(folder) = &node.folder else {
            return;
        };
        if !visited.insert(id) {
            return;
        }
        let name = folder
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let full = if prefix.is_empty() {
            name.to_owned()
        } else {
            format!("{prefix}/{name}")
        };
        paths.push((id, full.clone()));
        for child in &node.children {
            self.paths_from(*child, &full, paths, visited);
        }
    }

    /// `_collect_subfolders`: the given folders (when `include_parents`) and
    /// every descendant. The SDK returns `list(set(...))`; this keeps the
    /// first-seen order instead, so the result is deterministic.
    #[must_use]
    pub(in crate::toolkits) fn collect(&self, parents: &[i64], include_parents: bool) -> Vec<i64> {
        let mut out = if include_parents {
            parents.to_vec()
        } else {
            Vec::new()
        };
        let mut visited = HashSet::new();
        for parent in parents {
            self.descend(*parent, &mut out, &mut visited);
        }
        let mut seen = HashSet::new();
        out.retain(|id| seen.insert(*id));
        out
    }

    fn descend(&self, id: i64, out: &mut Vec<i64>, visited: &mut HashSet<i64>) {
        if !visited.insert(id) {
            return;
        }
        let Some(node) = self.nodes.get(&id) else {
            return;
        };
        for child in &node.children {
            out.push(*child);
            self.descend(*child, out, visited);
        }
    }
}

/// `_find_folders_by_name`: exact, or case-insensitive substring, matches in
/// folder order.
#[must_use]
pub(in crate::toolkits) fn find_by_name(folders: &[Value], name: &str, exact: bool) -> Vec<i64> {
    let lowered = name.to_lowercase();
    folders
        .iter()
        .filter(|folder| {
            let candidate = folder
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if exact {
                candidate == name
            } else {
                candidate.to_lowercase().contains(&lowered)
            }
        })
        .filter_map(|folder| folder_id(folder.get("id")))
        .collect()
}

/// `_parse_tests`: one list of labelled strings per test case.
#[must_use]
pub(in crate::toolkits) fn parse_tests(tests: &[Value]) -> Vec<Vec<String>> {
    tests
        .iter()
        .map(|test| {
            let nested = |member: &str, field: &str| match test.get(member) {
                None | Some(Value::Null) => "None".to_owned(),
                Some(object) => py_str_opt(object.get(field)),
            };
            vec![
                format!("Test ID: {}", py_str_opt(test.get("id"))),
                format!("Key: {}", py_str_opt(test.get("key"))),
                format!("Name: {}", py_str_opt(test.get("name"))),
                format!("Project ID: {}", nested("project", "id")),
                format!("Precondition: {}", py_str_opt(test.get("precondition"))),
                format!("Priority ID: {}", nested("priority", "id")),
                format!("Status ID: {}", nested("status", "id")),
                format!("Owner Account ID: {}", nested("owner", "accountId")),
            ]
        })
        .collect()
}

/// `str(_parse_tests(...))`: Python's repr of a list of string lists.
#[must_use]
pub(in crate::toolkits) fn parsed_tests_repr(tests: &[Value]) -> String {
    let rows = parse_tests(tests)
        .iter()
        .map(|row| crate::toolkits::families::python_repr::repr_str_list(row))
        .collect::<Vec<_>>();
    format!("[{}]", rows.join(", "))
}

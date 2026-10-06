//! `artifact_export.ArtifactExporter`: the wiki as artifacts.
//!
//! * `{wiki_id}/analysis/wiki_structure_{YYYYmmdd_HHMMSS}.json` — the
//!   structure with every page body, `json.dumps(…, indent=4)`
//!   (`ensure_ascii` on); sections without a generated page are dropped;
//! * `{wiki_id}/wiki_pages/README.md` — the index;
//! * `{wiki_id}/wiki_pages/{slug(section)}/{slug(page)}.md` — one per
//!   page; a FAILED page is an empty file, as in Python.
//!
//! `slug` is `_create_safe_filename`: drop everything but word characters,
//! whitespace and `-`, collapse `-`/whitespace runs to one `-`, strip `-`,
//! lower-case.
//!
//! DELIBERATE DIFFERENCES:
//!
//! * colliding page slugs in one directory get `-2`, `-3`, … (the
//!   README links and the structure JSON use the same names). Python wrote
//!   them to one file, so the later page silently replaced the earlier;
//! * an empty page slug is `page` and an empty section slug `section`.
//!   Python wrote `.md` (which its own `suffix == '.md'` test then skipped,
//!   losing the page) and put an unnamed section's pages next to the
//!   README;
//! * the artifact order is README, then the pages in structure order.
//!   Python listed the files in the temporary directory's own order
//!   (`rglob`), which differs between filesystems;
//! * the structure timestamp is UTC. Python used the local wall clock (the
//!   same in a UTC container).

use super::pyregex::{Flags, PyRe};
use super::spec::{WikiPage, WikiStructureSpec};
use crate::errors::{EngineError, ErrorType};
use crate::pyjson;
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};
use std::fmt::Write as _;
use std::sync::LazyLock;

/// One artifact of the engine result.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Artifact {
    pub name: String,
    /// `object_type` (only the manifest carries one).
    pub object_type: Option<String>,
    /// `type`: the MIME type.
    pub mime: String,
    pub data: String,
}

impl Artifact {
    /// The artifact as the result carries it (key order `name`,
    /// `object_type`, `type`, `data`).
    #[must_use]
    pub fn to_json(&self) -> Value {
        let mut map = Map::new();
        map.insert("name".into(), json!(self.name));
        if let Some(object_type) = &self.object_type {
            map.insert("object_type".into(), json!(object_type));
        }
        map.insert("type".into(), json!(self.mime));
        map.insert("data".into(), json!(self.data));
        Value::Object(map)
    }
}

fn regex_error(error: &super::pyregex::ReError) -> EngineError {
    EngineError::new(ErrorType::Runtime, error.to_string())
}

/// `_create_safe_filename`.
///
/// # Errors
///
/// Never in practice (two constant patterns).
pub fn safe_filename(title: &str) -> Result<String, EngineError> {
    static DROP: LazyLock<Result<PyRe, super::pyregex::ReError>> =
        LazyLock::new(|| PyRe::new(r"[^\w\s-]", Flags::NONE));
    static DASHES: LazyLock<Result<PyRe, super::pyregex::ReError>> =
        LazyLock::new(|| PyRe::new(r"[-\s]+", Flags::NONE));
    let drop = DROP.as_ref().map_err(regex_error)?;
    let collapse = DASHES.as_ref().map_err(regex_error)?;
    let kept = drop.sub(title, "").map_err(|e| regex_error(&e))?;
    let slug = collapse.sub(&kept, "-").map_err(|e| regex_error(&e))?;
    Ok(slug.trim_matches('-').to_lowercase())
}

/// `artifact_export.normalize_wiki_id(repository=…, branch=…)` as
/// `export_wiki` calls it on the agent's `repository_url`: `None` when the
/// URL has no `owner/repo` path.
#[must_use]
pub fn exporter_wiki_id(repository_url: &str, branch: &str) -> Option<String> {
    let path = if let Some((_, rest)) = repository_url.split_once("://") {
        rest.split_once('/')?.1.to_owned()
    } else if repository_url.contains('@') && repository_url.contains(':') {
        repository_url
            .split_once(':')
            .map_or("", |(_, p)| p)
            .to_owned()
    } else {
        repository_url.to_owned()
    };
    let path = path.trim_end_matches('/');
    let path = path.strip_suffix(".git").unwrap_or(path);
    if !path.contains('/') {
        return None;
    }
    let branch = if branch.is_empty() { "main" } else { branch };
    let mut repo_path = crate::graph::pystr::strip(path).to_owned();
    if repo_path.starts_with("git@") && repo_path.contains(':') {
        repo_path = repo_path
            .split_once(':')
            .map_or(String::new(), |(_, p)| p.to_owned());
    }
    let repo_path = repo_path.trim_end_matches('/');
    let repo_path = repo_path.strip_suffix(".git").unwrap_or(repo_path);
    (!repo_path.is_empty())
        .then(|| super::compose::normalize_wiki_id(&format!("{repo_path}:{branch}")))
}

/// The page matching `section#page`, else the first page with the
/// structure's page name (`_export_*`'s lookup).
fn matching_page<'p>(
    pages: &'p [WikiPage],
    by_id: &HashMap<&str, &'p WikiPage>,
    id: &str,
    name: &str,
) -> Option<&'p WikiPage> {
    by_id
        .get(id)
        .copied()
        .or_else(|| pages.iter().find(|p| p.title == name))
}

/// One exported page: its directory slug and file name.
struct Placed<'p> {
    section_index: usize,
    section_dir: String,
    file_name: String,
    page: &'p WikiPage,
}

/// The exporter of one wiki.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArtifactExporter {
    /// `{wiki_id}/` prefix of every name; none without a wiki id.
    pub wiki_id: Option<String>,
}

impl ArtifactExporter {
    fn prefixed(&self, subfolder: &str, name: &str) -> String {
        match &self.wiki_id {
            Some(id) => format!("{id}/{subfolder}/{name}"),
            None => format!("{subfolder}/{name}"),
        }
    }

    /// Place every page that has a match, with collision-free names.
    fn place<'p>(
        structure: &WikiStructureSpec,
        pages: &'p [WikiPage],
    ) -> Result<Vec<Placed<'p>>, EngineError> {
        let by_id: HashMap<&str, &WikiPage> =
            pages.iter().map(|p| (p.page_id.as_str(), p)).collect();
        let mut used: HashMap<String, HashSet<String>> = HashMap::new();
        let mut placed = Vec::new();
        for (section_index, section) in structure.sections.iter().enumerate() {
            let mut section_dir = safe_filename(&section.section_name)?;
            if section_dir.is_empty() {
                "section".clone_into(&mut section_dir);
            }
            for (page_index, spec) in section.pages.iter().enumerate() {
                let id = format!("{section_index}#{page_index}");
                let Some(page) = matching_page(pages, &by_id, &id, &spec.page_name) else {
                    continue;
                };
                let mut base = safe_filename(&page.title)?;
                if base.is_empty() {
                    "page".clone_into(&mut base);
                }
                let taken = used.entry(section_dir.clone()).or_default();
                let mut name = base.clone();
                let mut n = 2;
                while taken.contains(&name) {
                    name = format!("{base}-{n}");
                    n += 1;
                }
                taken.insert(name.clone());
                placed.push(Placed {
                    section_index,
                    section_dir: section_dir.clone(),
                    file_name: format!("{name}.md"),
                    page,
                });
            }
        }
        Ok(placed)
    }

    /// `export_wiki_artifacts(…, export_formats=["json", "markdown"])`.
    /// `timestamp` is `%Y%m%d_%H%M%S`.
    ///
    /// # Errors
    ///
    /// Never in practice (the slug patterns are constant).
    pub fn export(
        &self,
        structure: &WikiStructureSpec,
        pages: &[WikiPage],
        timestamp: &str,
    ) -> Result<Vec<Artifact>, EngineError> {
        let placed = Self::place(structure, pages)?;
        let mut artifacts = Vec::with_capacity(placed.len() + 2);

        // JSON: sections with at least one page.
        let mut sections_data = Vec::new();
        for (index, section) in structure.sections.iter().enumerate() {
            let section_pages: Vec<Value> = placed
                .iter()
                .filter(|p| p.section_index == index)
                .map(|p| json!({"page_name": p.file_name, "page_content": p.page.content}))
                .collect();
            if !section_pages.is_empty() {
                sections_data
                    .push(json!({"section_name": section.section_name, "pages": section_pages}));
            }
        }
        let wiki_data = json!({"wiki_title": structure.wiki_title, "sections": sections_data});
        artifacts.push(Artifact {
            name: self.prefixed("analysis", &format!("wiki_structure_{timestamp}.json")),
            object_type: None,
            mime: "application/json".to_owned(),
            data: pyjson::dumps_with(&wiki_data, Some(4), true),
        });

        // README.
        let mut readme = format!(
            "# {}\n\n{}\n\n## Wiki Structure\n\n",
            structure.wiki_title, structure.overview
        );
        for (index, section) in structure.sections.iter().enumerate() {
            let _ = write!(readme, "### {}/\n\n", section.section_name);
            for p in placed.iter().filter(|p| p.section_index == index) {
                let _ = writeln!(
                    readme,
                    "- [{}]({}/{})",
                    p.page.title, p.section_dir, p.file_name
                );
            }
            readme.push('\n');
        }
        readme.push_str("\n---\n\n*Generated by EliteA Wiki Toolkit*\n");
        artifacts.push(Artifact {
            name: self.prefixed("wiki_pages", "README.md"),
            object_type: None,
            mime: "text/markdown".to_owned(),
            data: readme,
        });

        for p in &placed {
            artifacts.push(Artifact {
                name: self.prefixed("wiki_pages", &format!("{}/{}", p.section_dir, p.file_name)),
                object_type: None,
                mime: "text/markdown".to_owned(),
                data: p.page.content.clone(),
            });
        }
        Ok(artifacts)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::wiki::spec::{PageSpec, PageStatus, SectionSpec};

    #[test]
    fn slugs_follow_python() {
        // python3 -c "import re;t='Architecture & Data Flow — Ünï_x';s=re.sub(r'[^\w\s-]','',t);print(re.sub(r'[-\s]+','-',s).strip('-').lower())"
        assert_eq!(
            safe_filename("Architecture & Data Flow — Ünï_x").unwrap(),
            "architecture-data-flow-ünï_x"
        );
        assert_eq!(
            safe_filename("Section 0 — Page 3").unwrap(),
            "section-0-page-3"
        );
        assert_eq!(safe_filename("!!!").unwrap(), "");
    }

    #[test]
    fn exporter_wiki_id_parses_the_url_forms() {
        assert_eq!(
            exporter_wiki_id("acme/Notes_Service", "main").as_deref(),
            Some("acme--notes-service--main")
        );
        assert_eq!(
            exporter_wiki_id("https://github.com/acme/x.git", "dev/1").as_deref(),
            Some("acme--x--dev-1")
        );
        assert_eq!(
            exporter_wiki_id("git@github.com:acme/x.git", "main").as_deref(),
            Some("acme--x--main")
        );
        assert_eq!(exporter_wiki_id("x", "main"), None);
        assert_eq!(exporter_wiki_id("https://host", "main"), None);
    }

    fn spec(name: &str) -> PageSpec {
        PageSpec {
            page_name: name.into(),
            page_order: 1,
            description: String::new(),
            content_focus: String::new(),
            rationale: String::new(),
            target_symbols: Vec::new(),
            target_docs: Vec::new(),
            target_folders: Vec::new(),
            key_files: Vec::new(),
            retrieval_query: String::new(),
            metadata: Map::new(),
        }
    }

    #[test]
    fn colliding_slugs_get_a_suffix_and_failed_pages_are_empty_files() {
        let structure = WikiStructureSpec {
            wiki_title: "T".into(),
            overview: "O".into(),
            sections: vec![SectionSpec {
                section_name: "S".into(),
                section_order: 1,
                description: String::new(),
                rationale: String::new(),
                pages: vec![spec("A b"), spec("A-b"), spec("!!")],
            }],
            total_pages: 3,
        };
        let page = |id: &str, title: &str, content: &str, status| WikiPage {
            page_id: id.into(),
            title: title.into(),
            content: content.into(),
            status,
        };
        let pages = [
            page("0#0", "A b", "one", PageStatus::Completed),
            page("0#1", "A-b", "", PageStatus::Failed),
            page("0#2", "!!", "three", PageStatus::Completed),
        ];
        let exporter = ArtifactExporter {
            wiki_id: Some("w".into()),
        };
        let artifacts = exporter
            .export(&structure, &pages, "20260101_000000")
            .unwrap();
        let names: Vec<&str> = artifacts.iter().map(|a| a.name.as_str()).collect();
        assert_eq!(
            names,
            [
                "w/analysis/wiki_structure_20260101_000000.json",
                "w/wiki_pages/README.md",
                "w/wiki_pages/s/a-b.md",
                "w/wiki_pages/s/a-b-2.md",
                "w/wiki_pages/s/page.md",
            ]
        );
        assert_eq!(artifacts[3].data, "");
        assert!(artifacts[1].data.contains("- [A-b](s/a-b-2.md)\n"));
    }
}

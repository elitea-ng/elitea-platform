//! The codebase tools (`deep_research/research_tools.py`'s
//! `create_codebase_tools`), as the LIVE workers built them: no networkx
//! graph and no FTS5 graph index, a `StorageQueryService` and a
//! `UnifiedRetriever` over the index.
//!
//! * `ask` (`DEEPWIKI_PROGRESSIVE_TOOLS` forced on): `search_symbols`,
//!   `get_relationships_tool`, `get_code`, `search_docs`, `query_graph`,
//!   `think`;
//! * `deep_research`: `search_codebase`, `get_symbol_relationships`,
//!   `search_graph`, `think`.
//!
//! The result texts are Python's, byte for byte. Where the Python live path
//! took its `.wiki.db` fallback (`get_code`, `get_symbol_relationships`,
//! `search_codebase`'s lexical branch), the same SQL runs against
//! PostgreSQL (see [`super::store`]).
//!
//! Ranges a model could abuse are clamped: `k` to 1..=100 (`search_docs`
//! and `search_graph`: 1..=50), `max_depth` to 0..=10, `max_lines` to
//! 1..=2000. Python took any value (a negative slice, an unbounded
//! `LIMIT`).
//!
//! Deliberate differences:
//! * `get_code` for a name the index does not hold answers "Symbol '…' not
//!   found" (Python's fallback path answered "Code graph not available");
//! * a storage failure is the tool's "… failed: …" text; Python swallowed
//!   some (`search_codebase`'s branches, `search_fts5`) as "no results".

use super::args::Args;
use super::embed::{self, Embedder};
use super::pyfmt;
use super::query::{Filters, QueryService};
use super::store::IndexStore;
use super::symbols::{self, DOC_SYMBOL_TYPES};
use crate::errors::EngineError;
use crate::runner::StopSignal;
use std::collections::HashSet;

/// `DEEPWIKI_MAX_DOC_RESULTS`' default (see [`super::Limits`]).
pub const MAX_DOC_RESULTS: usize = 3;

/// The ask tool set, in the order the model is offered it.
pub const ASK_TOOLS: &[&str] = &[
    "search_symbols",
    "get_relationships_tool",
    "get_code",
    "search_docs",
    "query_graph",
    "think",
];

/// The research codebase tools.
pub const RESEARCH_TOOLS: &[&str] = &[
    "search_codebase",
    "get_symbol_relationships",
    "search_graph",
    "think",
];

fn clamp(value: i64, low: i64, high: i64) -> usize {
    usize::try_from(value.clamp(low, high)).unwrap_or(0)
}

/// One document of a search (`langchain` `Document`'s fields the tools
/// read).
#[derive(Debug, Clone)]
struct Doc {
    content: String,
    source: String,
    symbol_name: String,
    symbol_type: String,
    start_line: String,
    end_line: String,
    search_source: &'static str,
}

/// The codebase tools over one index.
pub struct Codebase<'a, S: IndexStore> {
    pub store: &'a S,
    pub embedder: &'a Embedder,
    pub stop: &'a StopSignal,
    /// `search_codebase`'s documentation results at most
    /// ([`super::Limits::doc_results`]).
    pub doc_results: usize,
}

impl<S: IndexStore> Codebase<'_, S> {
    fn service(&self) -> QueryService<'_, S> {
        QueryService { store: self.store }
    }

    /// Run one codebase tool with validated arguments. A failure is the
    /// tool's text; only the stop line is an error.
    ///
    /// # Errors
    ///
    /// The stop line.
    pub async fn run(&self, tool: &str, args: &Args) -> Result<Option<String>, EngineError> {
        let (outcome, failure) = match tool {
            "search_symbols" => (self.search_symbols(args).await, "Symbol search failed"),
            "get_relationships_tool" => (
                self.get_relationships(args).await,
                "Relationship analysis failed",
            ),
            "get_code" => (self.get_code(args).await, "Code retrieval failed"),
            "search_docs" => (self.search_docs(args).await, "Documentation search failed"),
            "query_graph" => (self.query_graph(args).await, "Query failed"),
            "think" => (
                Ok(format!(
                    "\u{2713} Thinking recorded.\n\nReflection:\n{}",
                    args.str("reflection")
                )),
                "",
            ),
            "search_codebase" => (self.search_codebase(args).await, "Search failed"),
            "get_symbol_relationships" => (
                self.symbol_relationships(args).await,
                "Relationship analysis failed",
            ),
            "search_graph" => (self.search_graph(args).await, "Graph search failed"),
            _ => return Ok(None),
        };
        match outcome {
            Ok(text) => Ok(Some(text)),
            Err(error) if error == EngineError::cancelled() => Err(error),
            Err(error) => Ok(Some(format!("{failure}: {}", error.message))),
        }
    }

    async fn search_symbols(&self, args: &Args) -> Result<String, EngineError> {
        let query = args.str("query");
        let k = clamp(args.int("k"), 1, 100);
        let symbol_type = args.str("symbol_type");
        let types: HashSet<String> = if symbol_type.is_empty() {
            symbols::progressive_sorted()
                .into_iter()
                .map(str::to_owned)
                .collect()
        } else {
            let lowered = symbol_type.to_lowercase();
            if !symbols::is_progressive(&lowered) {
                return Ok(format!(
                    "Unsupported symbol_type '{symbol_type}'. Supported values: {}",
                    symbols::progressive_sorted().join(", ")
                ));
            }
            HashSet::from([lowered])
        };
        let prefix = args.str("file_prefix");
        let filters = Filters {
            symbol_types: Some(&types),
            exclude_types: Some(DOC_SYMBOL_TYPES),
            layer: None,
            path_prefix: (!prefix.is_empty()).then_some(prefix),
        };
        let service = self.service();
        let found = service.search(query, k, &filters).await?;
        if found.is_empty() {
            return Ok(format!("No symbols found for: {query}"));
        }
        let mut lines = vec![format!("Found {} symbols for: {query}\n", found.len())];
        for (index, result) in found.iter().enumerate() {
            let node = service.get_node(&result.node_id).await?;
            let docstring = node.map(|n| n.docstring).unwrap_or_default();
            let mut one_line = String::new();
            if !docstring.is_empty() {
                let first = crate::graph::pystr::strip(&docstring)
                    .split('\n')
                    .next()
                    .unwrap_or_default();
                let first = crate::graph::pystr::strip(first);
                one_line = if pyfmt::len(first) > 80 {
                    format!("{}...", pyfmt::head(first, 77))
                } else {
                    first.to_owned()
                };
            }
            let doc = if one_line.is_empty() {
                String::new()
            } else {
                format!(" \u{2014} {one_line}")
            };
            let layer = if result.layer.is_empty() {
                String::new()
            } else {
                format!(" [{}]", result.layer)
            };
            lines.push(format!(
                "{}. `{}` ({}{layer}) in {} [{} refs]{doc}",
                index + 1,
                result.symbol_name,
                result.symbol_type,
                result.rel_path,
                result.connections
            ));
        }
        Ok(lines.join("\n"))
    }

    async fn get_relationships(&self, args: &Args) -> Result<String, EngineError> {
        let symbol_name = args.str("symbol_name");
        let direction = args.str("direction");
        let max_depth = clamp(args.int("max_depth"), 0, 10);
        let Some((node_id, rels)) = self
            .service()
            .resolve_and_traverse(symbol_name, direction, max_depth, 50)
            .await?
        else {
            return Ok(format!(
                "Symbol '{symbol_name}' not found. Try search_symbols first."
            ));
        };
        if rels.is_empty() {
            return Ok(format!(
                "`{symbol_name}` has no relationships within {max_depth} hops."
            ));
        }
        let mut lines = vec![format!(
            "Relationships for `{symbol_name}` (node: `{node_id}`):\n"
        )];
        let lowered = symbol_name.to_lowercase();
        for r in &rels {
            let source_lower = r.source_name.to_lowercase();
            let outgoing = r.source_name == node_id
                || source_lower == lowered
                || source_lower.contains(&lowered);
            let (arrow, other, other_type) = if outgoing {
                ("\u{2192}", &r.target_name, &r.target_type)
            } else {
                ("\u{2190}", &r.source_name, &r.source_type)
            };
            let hop = if r.hop_distance > 1 {
                format!(" (hop {})", r.hop_distance)
            } else {
                String::new()
            };
            let kind = if other_type.is_empty() {
                String::new()
            } else {
                format!(" ({other_type})")
            };
            let via = if r.via.is_empty() {
                String::new()
            } else {
                format!(" via {}", r.via.join(", "))
            };
            lines.push(format!(
                "  {arrow} `{other}`{kind} [{}]{hop}{via}",
                r.relationship_type
            ));
        }
        Ok(lines.join("\n"))
    }

    async fn get_code(&self, args: &Args) -> Result<String, EngineError> {
        let symbol_name = args.str("symbol_name");
        let max_lines = clamp(args.int("max_lines"), 1, 2000);
        let mut row = self
            .store
            .name_rows(symbol_name, true, 1)
            .await?
            .into_iter()
            .next();
        if row.is_none() {
            row = self.store.shortest_like(symbol_name).await?;
        }
        let Some(row) = row else {
            return Ok(format!(
                "Symbol '{symbol_name}' not found. Check the exact name from search_symbols."
            ));
        };
        let mut content = if row.source_text.is_empty() {
            row.docstring.clone()
        } else {
            row.source_text.clone()
        };
        if crate::graph::pystr::strip(&content).is_empty() {
            if row.signature.is_empty() {
                return Ok(format!(
                    "### `{}` ({}) \u{2014} {}\n\nSource code not stored for this symbol.\nTip: use search_docs for documentation context.",
                    row.symbol_name, row.symbol_type, row.rel_path
                ));
            }
            content.clone_from(&row.signature);
        }
        let lines: Vec<&str> = content.split('\n').collect();
        let truncated = lines.len() > max_lines;
        let shown = lines[..lines.len().min(max_lines)].join("\n");
        let location = if row.start_line == 0 {
            String::new()
        } else {
            format!(":{}-{}", row.start_line, row.end_line)
        };
        let suffix = if truncated {
            format!("\n... (truncated at {max_lines} lines)")
        } else {
            String::new()
        };
        Ok(format!(
            "### `{}` ({}) \u{2014} {}{location}\n\n```\n{shown}\n```{suffix}",
            row.symbol_name, row.symbol_type, row.rel_path
        ))
    }

    /// `retriever_stack.search_repository(query, k, apply_expansion=False)`.
    async fn repository_docs(&self, query: &str, k: usize) -> Result<Vec<Doc>, EngineError> {
        if crate::graph::pystr::strip(query).is_empty() {
            return Ok(Vec::new());
        }
        let embedding = self.embedder.query(query, self.stop).await?;
        let rows = self
            .store
            .search_hybrid(query, embedding.as_deref(), k, 30, 30)
            .await?;
        Ok(rows
            .into_iter()
            .map(|row| Doc {
                content: row.node.source_text,
                source: row.node.rel_path,
                symbol_name: row.node.symbol_name,
                symbol_type: row.node.symbol_type,
                start_line: row.node.start_line.to_string(),
                end_line: row.node.end_line.to_string(),
                search_source: "vectorstore",
            })
            .collect())
    }

    /// `EmbeddingsFilter(k)` or the first `k`.
    async fn rerank(&self, query: &str, docs: Vec<Doc>, k: usize) -> Result<Vec<Doc>, EngineError> {
        let texts: Vec<String> = docs.iter().map(|d| d.content.clone()).collect();
        Ok(
            match embed::filter(self.embedder, query, &texts, k, self.stop).await? {
                Some(keep) => keep.into_iter().map(|i| docs[i].clone()).collect(),
                None => docs.into_iter().take(k).collect(),
            },
        )
    }

    async fn search_docs(&self, args: &Args) -> Result<String, EngineError> {
        let query = args.str("query");
        let k = clamp(args.int("k"), 1, 50);
        let docs = self.repository_docs(query, (k * 3).min(15)).await?;
        let docs = self.rerank(query, docs, k).await?;
        if docs.is_empty() {
            return Ok(format!("No documentation found for: {query}"));
        }
        let sections: Vec<String> = docs
            .iter()
            .enumerate()
            .map(|(i, d)| format!("### [{}] {}\n\n{}", i + 1, d.source, d.content))
            .collect();
        Ok(format!(
            "## Documentation: {query}\n\n{}",
            sections.join("\n\n---\n\n")
        ))
    }

    async fn query_graph(&self, args: &Args) -> Result<String, EngineError> {
        let expression = args.str("expression");
        let results = self.service().query(expression).await?;
        if results.is_empty() {
            return Ok(format!("No symbols matched query: {expression}"));
        }
        let mut lines = vec![format!(
            "## Query Results: `{expression}` ({} matches)\n",
            results.len()
        )];
        for r in &results {
            let location = if r.rel_path.is_empty() {
                "?"
            } else {
                &r.rel_path
            };
            let layer = if r.layer.is_empty() {
                String::new()
            } else {
                format!(" [{}]", r.layer)
            };
            let connections = if r.connections == 0 {
                String::new()
            } else {
                format!(" ({} connections)", r.connections)
            };
            lines.push(format!(
                "- **{}** ({}){layer} \u{2014} `{location}`{connections}",
                r.symbol_name, r.symbol_type
            ));
        }
        Ok(lines.join("\n"))
    }

    /// `re.split(r'\W+', query)`, parts of two characters or more.
    fn keywords(query: &str) -> Vec<String> {
        query
            .split(|c: char| !(c.is_alphanumeric() || c == '_'))
            .filter(|w| w.chars().count() >= 2)
            .map(str::to_owned)
            .collect()
    }

    async fn search_codebase(&self, args: &Args) -> Result<String, EngineError> {
        let query = args.str("query");
        let k = clamp(args.int("k"), 1, 100);
        let k_doc = k.min(self.doc_results);
        let mut all = if k_doc == 0 {
            Vec::new()
        } else {
            let docs = self.repository_docs(query, (k_doc * 4).min(20)).await?;
            self.rerank(query, docs, k_doc).await?
        };
        let k_code = k.max(10);
        let graph: Vec<Doc> = self
            .store
            .search_fts_any(&Self::keywords(query), k_code)
            .await?
            .into_iter()
            .filter_map(|node| {
                let content = if node.source_text.is_empty() {
                    node.docstring.clone()
                } else {
                    node.source_text.clone()
                };
                if crate::graph::pystr::strip(&content).is_empty() {
                    return None;
                }
                let line = |n: i32| if n == 0 { String::new() } else { n.to_string() };
                Some(Doc {
                    content,
                    source: if node.rel_path.is_empty() {
                        "unknown".to_owned()
                    } else {
                        node.rel_path.clone()
                    },
                    symbol_name: node.symbol_name.clone(),
                    symbol_type: if node.symbol_type.is_empty() {
                        "unknown".to_owned()
                    } else {
                        node.symbol_type.clone()
                    },
                    start_line: line(node.start_line),
                    end_line: line(node.end_line),
                    search_source: "unified_db_fts",
                })
            })
            .collect();
        // DEEPWIKI_HYBRID_FUSION is off: the legacy concatenation.
        let mut seen: HashSet<String> = all
            .iter()
            .filter(|d| !d.symbol_name.is_empty())
            .map(|d| d.symbol_name.to_lowercase())
            .collect();
        for doc in graph {
            let symbol = doc.symbol_name.to_lowercase();
            if !symbol.is_empty() && seen.insert(symbol) {
                all.push(doc);
            }
        }
        all.sort_by_key(|d| u8::from(d.search_source != "vectorstore"));
        all.truncate(k * 2);
        if all.is_empty() {
            return Ok(format!("No results found for query: {query}"));
        }
        let blocks: Vec<String> = all
            .iter()
            .enumerate()
            .map(|(i, d)| {
                format!(
                    "\n### [{}] {} ({})\n**Symbol:** `{}` ({})\n**Lines:** {}-{}\n\n```\n{}\n```\n",
                    i + 1,
                    d.source,
                    d.search_source,
                    d.symbol_name,
                    d.symbol_type,
                    d.start_line,
                    d.end_line,
                    d.content
                )
            })
            .collect();
        Ok(format!(
            "## Search Results for: {query}\n\nFound {} relevant items:\n{}",
            all.len(),
            blocks.join("\n---\n")
        ))
    }

    async fn symbol_relationships(&self, args: &Args) -> Result<String, EngineError> {
        let symbol_name = args.str("symbol_name");
        let mut rows = self.store.name_rows(symbol_name, true, 5).await?;
        if rows.is_empty() {
            rows = self.store.name_rows(symbol_name, false, 5).await?;
        }
        let Some(target) = rows.first() else {
            return Ok(format!("Symbol '{symbol_name}' not found in unified DB."));
        };
        let outgoing = self.store.joined_edges(&target.node_id, true, 50).await?;
        let incoming = self.store.joined_edges(&target.node_id, false, 50).await?;
        let name = &target.symbol_name;
        let mut lines = vec![
            format!("# Relationships for `{name}`\n"),
            format!("**Matched Node:** `{}`", target.node_id),
            format!(
                "**Outgoing:** {}, **Incoming:** {}\n",
                outgoing.len(),
                incoming.len()
            ),
        ];
        if rows.len() > 1 {
            let others: Vec<String> = rows[1..]
                .iter()
                .map(|r| format!("`{}`", r.symbol_name))
                .collect();
            lines.push(format!("**Other Matches:** {}\n", others.join(", ")));
        }
        if !outgoing.is_empty() {
            lines.push("\n## Outgoing Relationships".to_owned());
            for e in &outgoing {
                lines.push(format!(
                    "- `{name}` \u{2192} `{}` ({}) [{}]",
                    e.symbol_name, e.symbol_type, e.rel_type
                ));
            }
        }
        if !incoming.is_empty() {
            lines.push("\n## Incoming Relationships".to_owned());
            for e in &incoming {
                lines.push(format!(
                    "- `{}` ({}) \u{2192} `{name}` [{}]",
                    e.symbol_name, e.symbol_type, e.rel_type
                ));
            }
        }
        Ok(lines.join("\n"))
    }

    async fn search_graph(&self, args: &Args) -> Result<String, EngineError> {
        let query = args.str("query");
        let k = clamp(args.int("k"), 1, 50);
        let neighbors = args.bool("include_neighbors");
        let service = self.service();
        let filters = Filters {
            exclude_types: Some(DOC_SYMBOL_TYPES),
            ..Filters::default()
        };
        let mut matched = Vec::new();
        for result in service.search(query, k, &filters).await? {
            let node = service.get_node(&result.node_id).await?;
            let node = node.unwrap_or_default();
            let content = [
                node.source_text.as_str(),
                node.docstring.as_str(),
                result.docstring.as_str(),
                node.signature.as_str(),
            ]
            .into_iter()
            .find(|text| !text.is_empty())
            .unwrap_or_default()
            .to_owned();
            if crate::graph::pystr::strip(&content).is_empty() {
                continue;
            }
            let rel_path = if result.rel_path.is_empty() {
                node.rel_path.clone()
            } else {
                result.rel_path.clone()
            };
            matched.push((result, node, content, rel_path));
        }
        if matched.is_empty() {
            return Ok(format!("No symbols found matching: {query}"));
        }
        let mut sections = Vec::new();
        for (index, (result, node, content, rel_path)) in matched.iter().take(k).enumerate() {
            let mut section = vec![
                format!(
                    "### [{}] `{}` ({})",
                    index + 1,
                    result.symbol_name,
                    result.symbol_type
                ),
                format!(
                    "**File:** {rel_path}  **Lines:** {}-{}",
                    node.start_line, node.end_line
                ),
            ];
            let mut preview = pyfmt::head(content, 500).to_owned();
            if pyfmt::len(content) > 500 {
                preview.push_str("\n... (truncated, use search_codebase for full)");
            }
            section.push(format!("\n```\n{preview}\n```"));
            if neighbors && !result.node_id.is_empty() {
                let rels = service
                    .get_relationships(&result.node_id, "both", 1, 8)
                    .await?;
                if !rels.is_empty() {
                    section.push("\n**Relationships:**".to_owned());
                    for r in rels {
                        section.push(format!(
                            "  - `{}` -> `{}` [{}]",
                            r.source_name, r.target_name, r.relationship_type
                        ));
                    }
                }
            }
            sections.push(section.join("\n"));
        }
        Ok(format!(
            "## Graph Search: {query}\n\nFound {} symbols with relationships:\n{}",
            sections.len(),
            sections.join("\n\n---\n\n")
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keywords_split_on_non_word() {
        type C<'a> = Codebase<'a, super::super::store::ReplayIndex>;
        assert_eq!(
            C::keywords("note store, a save()"),
            vec!["note", "store", "save"]
        );
        assert_eq!(C::keywords("handle_search"), vec!["handle_search"]);
        assert_eq!(clamp(-5, 1, 100), 1);
        assert_eq!(clamp(i64::MAX, 1, 100), 100);
    }
}

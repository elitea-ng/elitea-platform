// Portions ported from Aider (https://github.com/Aider-AI/aider), aider/repomap.py @ 5dc9490bb35f9729ef2c95d00a19ccd30c26339c. Apache License 2.0. Copyright Paul Gauthier and Aider contributors.
//
// Ported: `RepoMap.get_ranked_tags` (the file graph, its weight multipliers,
// the personalization, PageRank, the rank handed from each file to the
// definitions it references) and the binary search of
// `get_ranked_tags_map_uncached`. Changes: the definitions and references
// come from the Inventory graph (tree-sitter entities and their resolved
// `calls` / `references` / … edges), not from Aider's tags queries; files
// that define or reference a focus symbol get personalization too; the focus files stay
// in the map (Aider leaves out the files already in its chat); ties sort by
// path and name ascending; the cut keeps the largest prefix that fits the
// budget, never one over it (Aider accepts 15% over), found by one scan
// instead of a binary search (a prefix's size here is additive); and the
// rendering is a path, then one line per symbol, not grep_ast's context
// tree.

//! The repo map: a ranked, token-budgeted overview of a repository, the
//! files most relevant to a task and their key symbols (Aider's repo map).
//!
//! [`rank`] builds a file graph from the code graph: a reference (a
//! `calls`, `references`, `extends`, `implements` or `uses` edge) from an
//! entity of file A to an identifier gives A an edge to every file that
//! defines that identifier, weighted as Aider weighs it (×10 for a focus
//! symbol, ×10 for a long `snake_case` / `camelCase` / kebab name, ×0.1 for a
//! `_private` one, ×0.1 for a name defined in more than five files, ×50 from
//! a focus file, × √ the number of references). Personalized `PageRank`
//! ([`elitea_graph_algos::pagerank`]) toward the focus files, the files
//! that define or reference a focus symbol and the files whose path names
//! one, ranks
//! the files; each file's rank is then spread over its outgoing edges onto
//! the (file, identifier) definitions they point at, and the definitions
//! are listed by that rank, then the files without a ranked definition.
//!
//! [`render`] keeps the longest prefix of that list whose text fits the
//! token budget (a token per four bytes, rounded up: the index cost
//! estimate's heuristic): a path, then one line per
//! symbol, its first source line, read through the caller's reader (the
//! desktop's is the confined workspace read).
//!
//! The same graph and focus give the same map, byte for byte: every map
//! here is ordered (no hash order), and `PageRank` sums in node order.

use crate::graph::Graph;
use crate::retrieval::query::citations_of;
use elitea_graph_algos::pagerank::{PageRankParams, personalized_pagerank};
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet};

/// The entity types a map lists as symbols.
const DEFINITION_TYPES: [&str; 12] = [
    "class",
    "function",
    "method",
    "interface",
    "struct",
    "trait",
    "enum",
    "type_alias",
    "macro",
    "constant",
    "module",
    "namespace",
];

/// The file nodes' types (a file without a symbol is still mapped).
const FILE_TYPES: [&str; 5] = [
    "file",
    "source_file",
    "document_file",
    "config_file",
    "web_file",
];

/// The relations that count as a reference from their source's file.
const REFERENCE_RELATIONS: [&str; 5] = ["calls", "references", "extends", "implements", "uses"];

/// The longest symbol line rendered, in characters.
const MAX_LINE_CHARS: usize = 120;

/// The bytes one token is estimated at.
const BYTES_PER_TOKEN: usize = 4;

/// What the map is biased toward.
#[derive(Debug, Clone, Copy, Default)]
pub struct Focus<'a> {
    /// Workspace-relative files (or directories: every file under one) the
    /// task is about.
    pub paths: &'a [String],
    /// Identifiers the task names.
    pub symbols: &'a [String],
}

impl Focus<'_> {
    fn has_path(&self, path: &str) -> bool {
        self.paths.iter().any(|focus| {
            let focus = focus.trim_matches('/');
            !focus.is_empty()
                && (path == focus
                    || path
                        .strip_prefix(focus)
                        .is_some_and(|rest| rest.starts_with('/')))
        })
    }

    fn names(&self, ident: &str) -> bool {
        self.symbols
            .iter()
            .any(|symbol| symbol == ident || symbol.eq_ignore_ascii_case(ident))
    }
}

/// One symbol of the map.
#[derive(Debug, Clone, PartialEq)]
pub struct Symbol {
    pub path: String,
    pub name: String,
    pub entity_type: String,
    /// The first line of its span (1-based), when known.
    pub line: Option<i64>,
    /// The stored signature, the line's fallback.
    pub signature: Option<String>,
}

/// One entry of the ranked list: a symbol, or a file with none ranked.
#[derive(Debug, Clone, PartialEq)]
pub enum Entry {
    Symbol(Symbol),
    File(String),
}

impl Entry {
    #[must_use]
    pub fn path(&self) -> &str {
        match self {
            Self::Symbol(symbol) => &symbol.path,
            Self::File(path) => path,
        }
    }
}

/// The ranked list [`render`] cuts.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Ranking {
    pub entries: Vec<Entry>,
}

/// One definition: a symbol's entity cited in one file.
struct Definition<'g> {
    name: &'g str,
    entity_type: &'g str,
    line: Option<i64>,
    signature: Option<&'g str>,
}

fn text<'g>(node: &'g Map<String, Value>, key: &str) -> Option<&'g str> {
    node.get(key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
}

/// Aider's "long identifier" test: `snake_case`, kebab or `camelCase`, at
/// least eight characters.
fn is_long_identifier(ident: &str) -> bool {
    let alpha = ident.chars().any(char::is_alphabetic);
    let snake = ident.contains('_') && alpha;
    let kebab = ident.contains('-') && alpha;
    let camel = ident.chars().any(char::is_uppercase) && ident.chars().any(char::is_lowercase);
    (snake || kebab || camel) && ident.chars().count() >= 8
}

/// A count as a float (counts stay far below 2^52).
#[allow(clippy::cast_precision_loss)]
fn count_f64(n: usize) -> f64 {
    n as f64
}

/// Rank `graph`'s files and symbols for `focus`. A file `admit` refuses
/// (a denied path) is left out with everything defined or referenced in
/// it.
#[must_use]
#[allow(clippy::too_many_lines)]
pub fn rank(graph: &Graph, focus: &Focus<'_>, admit: impl Fn(&str) -> bool) -> Ranking {
    // Every file the map may name, and the definitions in each.
    let mut files: BTreeSet<&str> = BTreeSet::new();
    // ident → the files defining it.
    let mut defined_in: BTreeMap<&str, BTreeSet<&str>> = BTreeMap::new();
    // (file, ident) → its definitions, in node order.
    let mut definitions: BTreeMap<(&str, &str), Vec<Definition<'_>>> = BTreeMap::new();
    for (_, node) in graph.nodes() {
        let Some(kind) = text(node, "type") else {
            continue;
        };
        let definition = DEFINITION_TYPES.contains(&kind);
        if !definition && !FILE_TYPES.contains(&kind) {
            continue;
        }
        for citation in citations_of(node) {
            let Some(path) = citation.get("file_path").and_then(Value::as_str) else {
                continue;
            };
            if path.is_empty() || !admit(path) {
                continue;
            }
            files.insert(path);
            if !definition {
                continue;
            }
            let Some(name) = text(node, "name") else {
                continue;
            };
            defined_in.entry(name).or_default().insert(path);
            definitions
                .entry((path, name))
                .or_default()
                .push(Definition {
                    name,
                    entity_type: kind,
                    line: citation.get("line_start").and_then(Value::as_i64),
                    signature: text(node, "signature"),
                });
        }
    }

    // ident → the referencing file of each reference.
    let mut referenced_from: BTreeMap<&str, Vec<&str>> = BTreeMap::new();
    for (source, target, edge) in graph.edges() {
        let relation = edge
            .get("relation_type")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if !REFERENCE_RELATIONS.contains(&relation) {
            continue;
        }
        let Some(target) = graph.node(target) else {
            continue;
        };
        let Some(ident) = text(target, "name").filter(|name| defined_in.contains_key(name)) else {
            continue;
        };
        // The file the relation was found in, else the source's own.
        let found_in = edge
            .get("discovered_in_file")
            .and_then(Value::as_str)
            .filter(|path| !path.is_empty())
            .or_else(|| {
                graph
                    .node(source)
                    .and_then(|node| citations_of(node).into_iter().next())
                    .and_then(|citation| citation.get("file_path"))
                    .and_then(Value::as_str)
            });
        let Some(referencer) = found_in.filter(|path| !path.is_empty() && admit(path)) else {
            continue;
        };
        files.insert(referencer);
        referenced_from.entry(ident).or_default().push(referencer);
    }
    if files.is_empty() {
        return Ranking::default();
    }
    // Aider: a graph without references ranks by definitions alone.
    if referenced_from.is_empty() {
        referenced_from = defined_in
            .iter()
            .map(|(ident, definers)| (*ident, definers.iter().copied().collect()))
            .collect();
    }

    let nodes: Vec<&str> = files.iter().copied().collect();
    let node_of = |path: &str| -> u32 {
        let position = nodes.binary_search(&path).unwrap_or_default();
        u32::try_from(position).unwrap_or(u32::MAX)
    };

    // The personalization (Aider's 100 / files per reason), here also for
    // the files around a focus symbol: those defining it and those
    // referencing it (whose rank then flows, ×10, into its definition).
    let personalize = 100.0 / count_f64(nodes.len());
    let around_focus: BTreeSet<&str> = defined_in
        .iter()
        .filter(|(ident, _)| focus.names(ident))
        .flat_map(|(ident, definers)| {
            definers
                .iter()
                .chain(referenced_from.get(ident).into_iter().flatten())
                .copied()
        })
        .collect();
    let personalization: Vec<f64> = nodes
        .iter()
        .map(|path| {
            let mut mass: f64 = 0.0;
            if focus.has_path(path) {
                mass += personalize;
            }
            // A path component (or the file name without its extension)
            // that names a focus symbol.
            let name = path.rsplit('/').next().unwrap_or(path);
            let stem = name.rsplit_once('.').map_or(name, |(stem, _)| stem);
            if path
                .split('/')
                .chain(std::iter::once(stem))
                .any(|part| focus.symbols.iter().any(|symbol| symbol == part))
            {
                mass += personalize;
            }
            if around_focus.contains(path) {
                mass = mass.max(personalize);
            }
            mass
        })
        .collect();

    // The file graph: (source, target, weight, ident) in a fixed order.
    let mut edges: Vec<(u32, u32, f64, &str)> = Vec::new();
    for (ident, definers) in &defined_in {
        if referenced_from.contains_key(ident) {
            continue;
        }
        // A small self-edge for a definition nothing references.
        for definer in definers {
            let node = node_of(definer);
            edges.push((node, node, 0.1, ident));
        }
    }
    for (ident, referencers) in &referenced_from {
        let Some(definers) = defined_in.get(ident) else {
            continue;
        };
        let mut mul = 1.0;
        if focus.names(ident) {
            mul *= 10.0;
        }
        if is_long_identifier(ident) {
            mul *= 10.0;
        }
        if ident.starts_with('_') {
            mul *= 0.1;
        }
        if definers.len() > 5 {
            mul *= 0.1;
        }
        let mut counts: BTreeMap<&str, usize> = BTreeMap::new();
        for referencer in referencers {
            *counts.entry(referencer).or_default() += 1;
        }
        for (referencer, count) in counts {
            let from_focus = if focus.has_path(referencer) {
                50.0
            } else {
                1.0
            };
            let weight = mul * from_focus * count_f64(count).sqrt();
            for definer in definers {
                edges.push((node_of(referencer), node_of(definer), weight, ident));
            }
        }
    }

    let weighted: Vec<(u32, u32, f64)> = edges.iter().map(|&(s, t, w, _)| (s, t, w)).collect();
    let ranks = personalized_pagerank(
        nodes.len(),
        &weighted,
        &personalization,
        &PageRankParams::default(),
    );

    // Each file's rank over its outgoing edges, onto (file, ident).
    let mut out_weight = vec![0.0f64; nodes.len()];
    for &(source, _, weight, _) in &edges {
        out_weight[source as usize] += weight;
    }
    let mut ranked: BTreeMap<(&str, &str), f64> = BTreeMap::new();
    for &(source, target, weight, ident) in &edges {
        let source = source as usize;
        if out_weight[source] > 0.0 {
            *ranked.entry((nodes[target as usize], ident)).or_default() +=
                ranks[source] * weight / out_weight[source];
        }
    }
    let mut ranked: Vec<((&str, &str), f64)> = ranked.into_iter().collect();
    // By rank, ties by (path, ident): the BTreeMap order, kept by the
    // stable sort.
    ranked.sort_by(|a, b| b.1.total_cmp(&a.1));

    let mut entries = Vec::new();
    let mut listed: BTreeSet<&str> = BTreeSet::new();
    for ((path, ident), _) in &ranked {
        for definition in definitions.get(&(*path, *ident)).into_iter().flatten() {
            listed.insert(path);
            entries.push(Entry::Symbol(Symbol {
                path: (*path).to_owned(),
                name: definition.name.to_owned(),
                entity_type: definition.entity_type.to_owned(),
                line: definition.line,
                signature: definition.signature.map(str::to_owned),
            }));
        }
    }
    // Then the files with nothing ranked, by their own rank.
    let mut by_rank: Vec<usize> = (0..nodes.len()).collect();
    by_rank.sort_by(|a, b| ranks[*b].total_cmp(&ranks[*a]));
    for node in by_rank {
        if !listed.contains(nodes[node]) {
            entries.push(Entry::File(nodes[node].to_owned()));
        }
    }
    Ranking { entries }
}

/// A rendered map.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RenderedMap {
    pub text: String,
    /// Its estimated tokens (a token per four bytes, rounded up).
    pub tokens: usize,
    /// How many entries of the ranking it shows.
    pub entries: usize,
    pub files: usize,
}

/// The estimated tokens of `text`.
#[must_use]
pub fn estimate_tokens(text: &str) -> usize {
    text.len().div_ceil(BYTES_PER_TOKEN)
}

/// Reads a file's lines once for every render of the search.
struct Lines<'r> {
    read: &'r mut dyn FnMut(&str) -> Option<String>,
    files: BTreeMap<String, Option<Vec<String>>>,
}

impl Lines<'_> {
    fn line(&mut self, path: &str, line: i64) -> Option<String> {
        let index = usize::try_from(line).ok()?.checked_sub(1)?;
        if !self.files.contains_key(path) {
            let lines = (self.read)(path).map(|text| text.lines().map(str::to_owned).collect());
            self.files.insert(path.to_owned(), lines);
        }
        let found = self.files.get(path)?.as_ref()?.get(index)?.trim();
        (!found.is_empty()).then(|| found.to_owned())
    }
}

fn clip(line: &str) -> String {
    if line.chars().count() <= MAX_LINE_CHARS {
        return line.to_owned();
    }
    let mut cut: String = line.chars().take(MAX_LINE_CHARS - 1).collect();
    cut.push('…');
    cut
}

/// One symbol's line: its first source line, else its stored signature,
/// else its kind and name.
fn symbol_line(symbol: &Symbol, lines: &mut Lines<'_>) -> String {
    let shown = symbol
        .line
        .and_then(|line| lines.line(&symbol.path, line))
        .or_else(|| symbol.signature.clone())
        .unwrap_or_else(|| format!("{} {}", symbol.entity_type, symbol.name));
    match symbol.line {
        Some(line) if line > 0 => format!("  {line}: {}\n", clip(&shown)),
        _ => format!("  {}\n", clip(&shown)),
    }
}

/// The map of the first `count` entries: the files in the order their
/// first entry is ranked, each file's symbols in line order. Its length is
/// the sum of the entries' [`entry_cost`]s: the order does not change it.
fn render_prefix(entries: &[Entry], count: usize, lines: &mut Lines<'_>) -> (String, usize) {
    let mut order: Vec<&str> = Vec::new();
    let mut symbols: BTreeMap<&str, Vec<&Symbol>> = BTreeMap::new();
    for entry in &entries[..count] {
        let path = entry.path();
        if !symbols.contains_key(path) {
            order.push(path);
            symbols.insert(path, Vec::new());
        }
        if let Entry::Symbol(symbol) = entry
            && let Some(list) = symbols.get_mut(path)
        {
            list.push(symbol);
        }
    }
    let mut out = String::new();
    for path in &order {
        out.push_str(path);
        out.push('\n');
        let mut list = symbols.remove(path).unwrap_or_default();
        list.sort_by_key(|symbol| (symbol.line.unwrap_or(i64::MAX), symbol.name.as_str()));
        for symbol in list {
            out.push_str(&symbol_line(symbol, lines));
        }
    }
    (out, order.len())
}

/// The bytes `entry` adds to a map that already names the files in
/// `named`: its file's path line when the file is new, and its symbol line.
fn entry_cost<'e>(entry: &'e Entry, named: &mut BTreeSet<&'e str>, lines: &mut Lines<'_>) -> usize {
    let header = if named.insert(entry.path()) {
        entry.path().len() + 1
    } else {
        0
    };
    header
        + match entry {
            Entry::Symbol(symbol) => symbol_line(symbol, lines).len(),
            Entry::File(_) => 0,
        }
}

/// The longest prefix of `ranking` that fits `max_tokens`, rendered under
/// `heading` (counted in the budget). `read` returns a file's text (`None`
/// when it cannot be read: the symbol shows its stored signature then);
/// each file is read at most once, and only the files of the entries
/// considered.
///
/// Aider binary-searches the prefix length, rendering each guess. A
/// prefix's size here is the sum of its entries' costs (the rendering only
/// reorders), so one scan in rank order finds the same largest prefix and
/// reads no file past the cut.
#[must_use]
pub fn render(
    ranking: &Ranking,
    heading: &str,
    max_tokens: usize,
    read: &mut dyn FnMut(&str) -> Option<String>,
) -> RenderedMap {
    let mut lines = Lines {
        read,
        files: BTreeMap::new(),
    };
    let budget = max_tokens.saturating_mul(BYTES_PER_TOKEN);
    let mut size = heading.len();
    let mut count = 0;
    let mut named = BTreeSet::new();
    for entry in &ranking.entries {
        let cost = entry_cost(entry, &mut named, &mut lines);
        if size + cost > budget {
            break;
        }
        size += cost;
        count += 1;
    }
    let (body, files) = render_prefix(&ranking.entries, count, &mut lines);
    let text = format!("{heading}{body}");
    debug_assert_eq!(text.len(), size);
    RenderedMap {
        tokens: estimate_tokens(&text),
        text,
        entries: count,
        files,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::Citation;
    use serde_json::json;

    fn cite(path: &str, line: i64) -> Citation {
        Citation {
            file_path: path.to_owned(),
            line_start: Some(line),
            line_end: Some(line + 2),
            source_toolkit: Some("workspace".to_owned()),
            doc_id: None,
            content_hash: None,
        }
    }

    fn define(graph: &mut Graph, id: &str, name: &str, kind: &str, path: &str, line: i64) {
        graph.add_entity(id, name, kind, Some(&cite(path, line)), None);
    }

    fn relate(graph: &mut Graph, from: &str, to: &str, relation: &str, file: &str) {
        let properties = json!({"discovered_in_file": file});
        assert!(graph.add_relation(from, to, relation, properties.as_object()));
    }

    /// A small repository: `claim.rs` defines `claim_turn`, called from
    /// `turn.rs` and `session.rs`; `util.rs` defines helpers everyone
    /// calls; `secret/keys.rs` is a denied file.
    fn fixture() -> Graph {
        let mut g = Graph::new();
        for (path, file_id) in [
            ("src/claim.rs", "f_claim"),
            ("src/turn.rs", "f_turn"),
            ("src/session.rs", "f_session"),
            ("src/util.rs", "f_util"),
            ("src/ui/view.rs", "f_view"),
            ("secret/keys.rs", "f_keys"),
            ("README.md", "f_readme"),
        ] {
            g.add_entity(file_id, path, "source_file", Some(&cite(path, 1)), None);
        }
        define(
            &mut g,
            "claim_turn",
            "claim_turn",
            "function",
            "src/claim.rs",
            3,
        );
        define(
            &mut g,
            "ClaimGuard",
            "ClaimGuard",
            "struct",
            "src/claim.rs",
            10,
        );
        define(&mut g, "run_turn", "run_turn", "function", "src/turn.rs", 5);
        define(
            &mut g,
            "open_session",
            "open_session",
            "function",
            "src/session.rs",
            2,
        );
        define(
            &mut g,
            "fmt_bytes",
            "fmt_bytes",
            "function",
            "src/util.rs",
            1,
        );
        define(&mut g, "log_line", "log_line", "function", "src/util.rs", 7);
        define(
            &mut g,
            "render_view",
            "render_view",
            "function",
            "src/ui/view.rs",
            4,
        );
        define(
            &mut g,
            "load_keys",
            "load_keys",
            "function",
            "secret/keys.rs",
            1,
        );
        relate(&mut g, "run_turn", "claim_turn", "calls", "src/turn.rs");
        relate(
            &mut g,
            "run_turn",
            "ClaimGuard",
            "references",
            "src/turn.rs",
        );
        relate(
            &mut g,
            "open_session",
            "claim_turn",
            "calls",
            "src/session.rs",
        );
        for caller in ["run_turn", "open_session", "render_view", "claim_turn"] {
            let file = match caller {
                "run_turn" => "src/turn.rs",
                "open_session" => "src/session.rs",
                "render_view" => "src/ui/view.rs",
                _ => "src/claim.rs",
            };
            relate(&mut g, caller, "fmt_bytes", "calls", file);
            relate(&mut g, caller, "log_line", "calls", file);
        }
        relate(
            &mut g,
            "render_view",
            "load_keys",
            "calls",
            "src/ui/view.rs",
        );
        relate(&mut g, "load_keys", "fmt_bytes", "calls", "secret/keys.rs");
        g
    }

    fn admit(path: &str) -> bool {
        !path.starts_with("secret/")
    }

    fn source(path: &str) -> Option<String> {
        let lines = match path {
            "src/claim.rs" => {
                "use x;\n\npub fn claim_turn(id: u64) -> Result<ClaimGuard> {\n}\n\n\n\n\n\npub struct ClaimGuard {\n"
            }
            "src/turn.rs" => "\n\n\n\npub async fn run_turn() {\n",
            _ => return None,
        };
        Some(lines.to_owned())
    }

    fn symbols(ranking: &Ranking) -> Vec<(&str, &str)> {
        ranking
            .entries
            .iter()
            .filter_map(|entry| match entry {
                Entry::Symbol(symbol) => Some((symbol.path.as_str(), symbol.name.as_str())),
                Entry::File(_) => None,
            })
            .collect()
    }

    #[test]
    fn focus_symbol_file_ranks_first() {
        let graph = fixture();
        // Unfocused, the helpers everyone calls lead.
        let plain = rank(&graph, &Focus::default(), admit);
        assert_eq!(plain.entries[0].path(), "src/util.rs", "{plain:?}");
        // Focused on the claim, its file leads.
        let symbols_named = ["claim_turn".to_owned()];
        let focus = Focus {
            paths: &[],
            symbols: &symbols_named,
        };
        let focused = rank(&graph, &focus, admit);
        assert_eq!(
            symbols(&focused)[0],
            ("src/claim.rs", "claim_turn"),
            "{focused:?}"
        );
        // A focus path leads too: what it references comes first.
        let paths = ["src/ui".to_owned()];
        let by_path = rank(
            &graph,
            &Focus {
                paths: &paths,
                symbols: &[],
            },
            admit,
        );
        assert_eq!(by_path.entries[0].path(), "src/util.rs", "{by_path:?}");
        // Every file appears once at least, the README (no symbol) last.
        let files: BTreeSet<&str> = focused.entries.iter().map(Entry::path).collect();
        assert_eq!(files.len(), 6, "{files:?}");
        assert_eq!(focused.entries.last().map(Entry::path), Some("README.md"));
    }

    #[test]
    fn denied_files_are_left_out() {
        let graph = fixture();
        let symbols_named = ["load_keys".to_owned()];
        let focus = Focus {
            paths: &[],
            symbols: &symbols_named,
        };
        let ranking = rank(&graph, &focus, admit);
        assert!(
            ranking.entries.iter().all(|entry| admit(entry.path())),
            "{ranking:?}"
        );
        let map = render(&ranking, "", 8192, &mut source);
        assert!(!map.text.contains("secret/") && !map.text.contains("load_keys"));
    }

    #[test]
    fn deterministic_for_the_same_graph_and_focus() {
        let graph = fixture();
        let symbols_named = ["ClaimGuard".to_owned()];
        let focus = Focus {
            paths: &[],
            symbols: &symbols_named,
        };
        let first = render(&rank(&graph, &focus, admit), "", 2048, &mut source);
        for _ in 0..3 {
            let again = render(&rank(&graph, &focus, admit), "", 2048, &mut source);
            assert_eq!(first, again);
        }
    }

    #[test]
    fn map_fits_token_budget() {
        let graph = fixture();
        let symbols_named = ["claim_turn".to_owned()];
        let focus = Focus {
            paths: &[],
            symbols: &symbols_named,
        };
        let ranking = rank(&graph, &focus, admit);
        let heading = "Repository map:\n";
        let whole = render(&ranking, heading, 8192, &mut source);
        assert_eq!(whole.entries, ranking.entries.len());
        assert!(
            whole
                .text
                .contains("  3: pub fn claim_turn(id: u64) -> Result<ClaimGuard> {")
        );
        // A symbol with no readable line shows its kind and name.
        assert!(
            whole.text.contains("  1: function fmt_bytes"),
            "{}",
            whole.text
        );
        for budget in [0, 5, 10, 20, 40, whole.tokens - 1] {
            let map = render(&ranking, heading, budget, &mut source);
            assert!(
                map.tokens <= budget || map.entries == 0,
                "{budget}: {map:?}"
            );
            assert!(map.entries < ranking.entries.len());
            assert!(map.text.len() <= budget * 4 || map.text == heading);
        }
    }

    /// The property Aider's binary search over the prefix length relies on,
    /// and the scan here uses: a longer prefix renders longer, and the map
    /// is the longest prefix that fits.
    #[test]
    fn binary_search_is_monotonic() {
        let graph = fixture();
        let ranking = rank(&graph, &Focus::default(), admit);
        let mut lines = Lines {
            read: &mut source,
            files: BTreeMap::new(),
        };
        let sizes: Vec<usize> = (0..=ranking.entries.len())
            .map(|count| render_prefix(&ranking.entries, count, &mut lines).0.len())
            .collect();
        assert!(sizes.windows(2).all(|pair| pair[0] < pair[1]), "{sizes:?}");
        // The prefix kept is the largest that fits: one more entry would
        // not.
        for budget in 1..=sizes[sizes.len() - 1].div_ceil(4) {
            let map = render(&ranking, "", budget, &mut source);
            assert_eq!(map.text.len(), sizes[map.entries]);
            assert!(sizes[map.entries] <= budget * 4);
            if map.entries < ranking.entries.len() {
                assert!(sizes[map.entries + 1] > budget * 4, "{budget}");
            }
        }
    }

    #[test]
    fn identifier_weights_follow_aider() {
        assert!(is_long_identifier("claim_turn"));
        assert!(is_long_identifier("ClaimGuard"));
        assert!(is_long_identifier("repo-map-x"));
        assert!(!is_long_identifier("run_it"));
        assert!(!is_long_identifier("lowercase"));
    }
}

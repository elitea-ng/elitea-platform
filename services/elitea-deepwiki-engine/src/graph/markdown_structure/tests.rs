use super::*;

fn node(
    graph: &mut CodeGraph,
    id: &str,
    name: &str,
    symbol_type: &str,
    rel_path: &str,
    text: &str,
) {
    graph.add_node(
        id,
        NodeData {
            symbol_name: name.to_owned(),
            symbol_type: symbol_type.to_owned().into(),
            rel_path: rel_path.into(),
            source_text: text.to_owned(),
            ..NodeData::default()
        },
    );
}

// The same graph through `wire_markdown_structure` in Python gives these
// stats, synthesized documents and edges (in networkx edge order).
#[test]
#[allow(clippy::too_many_lines)] // one graph, one expected edge list
fn contains_and_references_match_the_python_pass() {
    let mut graph = CodeGraph::new();
    node(
        &mut graph,
        "go::server::Serve",
        "Serve",
        "function",
        "src/server.go",
        "",
    );
    node(
        &mut graph,
        "go::server::Config",
        "Config",
        "struct",
        "src/server.go",
        "",
    );
    node(&mut graph, "py::a::Config", "Config", "class", "a.py", "");
    node(&mut graph, "py::b::Config", "Config", "class", "b.py", "");
    node(
        &mut graph,
        "doc::notes",
        "Notes",
        "markdown_section",
        "notes.txt",
        "",
    );
    node(
        &mut graph,
        "markdown::README::Intro",
        "Intro",
        "markdown_section",
        "docs/README.md",
        "[File: docs/README.md]\nSee [server](../src/server.go#L1), [ext](https://x.y), [self](README.md) and `Serve`, `pkg.Config`, `Notes`, `Missing`.",
    );
    node(
        &mut graph,
        "markdown::README::Usage",
        "Usage",
        "Markdown_Section",
        "docs/README.md",
        "Run [it](./../src/server.go) `Serve`",
    );
    node(
        &mut graph,
        "markdown::GUIDE::__doc__",
        "GUIDE",
        "markdown_document",
        "GUIDE.md",
        "",
    );
    node(
        &mut graph,
        "markdown::GUIDE::A",
        "A",
        "markdown_section",
        "GUIDE.md",
        "",
    );
    node(
        &mut graph,
        "markdown::orphan::A",
        "A",
        "markdown_section",
        "",
        "`Serve`",
    );

    let stats = wire_markdown_structure(&mut graph);
    assert_eq!(
        stats,
        MarkdownStats {
            markdown_nodes: 6,
            documents_synthesized: 2,
            contains_edges: 4,
            references_edges: 7,
        }
    );
    let synthesized = graph.node("markdown_document::docs/README.md").unwrap();
    assert_eq!(
        (
            synthesized.symbol_name.as_str(),
            synthesized.file_name.as_str(),
            synthesized.language.as_str(),
            &*synthesized.analysis_level,
            synthesized.parameters.as_str(),
            synthesized.return_type.as_str(),
        ),
        (
            "README.md",
            "README.md",
            "markdown",
            "documentation",
            "[]",
            "markdown"
        )
    );
    assert!(graph.has_node("markdown_document::notes.txt"));

    let edges: Vec<String> = graph
        .edges()
        .map(|e| {
            format!(
                "{} {} {} {:?} {}",
                e.source,
                e.target,
                e.data.rel_type,
                e.data.raw_similarity,
                crate::pyjson::dumps(&Value::Object(e.data.annotations.to_map()))
            )
        })
        .collect();
    let md_link = r#"{"confidence": "EXTRACTED", "matcher": "md_link"}"#;
    let backtick = r#"{"confidence": "EXTRACTED", "matcher": "backtick"}"#;
    let contains = r#"{"confidence": "EXTRACTED"}"#;
    assert_eq!(
        edges,
        [
            format!("markdown::README::Intro go::server::Config references Some(0.95) {md_link}"),
            format!(
                "markdown::README::Intro markdown_document::docs/README.md references Some(0.95) {md_link}"
            ),
            format!("markdown::README::Intro go::server::Serve references Some(0.95) {backtick}"),
            format!("markdown::README::Intro py::a::Config references Some(0.95) {backtick}"),
            format!("markdown::README::Usage go::server::Config references Some(0.95) {md_link}"),
            format!("markdown::README::Usage go::server::Serve references Some(0.95) {backtick}"),
            format!("markdown::GUIDE::__doc__ markdown::GUIDE::A contains None {contains}"),
            format!("markdown::orphan::A go::server::Serve references Some(0.95) {backtick}"),
            format!("markdown_document::notes.txt doc::notes contains None {contains}"),
            format!(
                "markdown_document::docs/README.md markdown::README::Intro contains None {contains}"
            ),
            format!(
                "markdown_document::docs/README.md markdown::README::Usage contains None {contains}"
            ),
        ]
    );
    let first = graph.edges().next().unwrap().data;
    assert_eq!(
        (
            &*first.edge_class,
            first.language.as_str(),
            &*first.created_by
        ),
        ("doc", "markdown", "markdown_structure")
    );

    // A second run adds nothing: every edge is already there.
    let again = wire_markdown_structure(&mut graph);
    assert_eq!(
        (
            again.contains_edges,
            again.references_edges,
            again.documents_synthesized
        ),
        (0, 0, 0)
    );
}

#[test]
fn links_resolve_like_posixpath() {
    assert_eq!(normalize_link("../src/a.go", "docs"), "src/a.go");
    assert_eq!(normalize_link("./a.md", ""), "a.md");
    assert_eq!(normalize_link("/abs/x", "docs"), "/abs/x");
    assert_eq!(normalize_link("mailto:x", "docs"), "");
    assert_eq!(normalize_link("../../../x", "a"), "../../x");
}

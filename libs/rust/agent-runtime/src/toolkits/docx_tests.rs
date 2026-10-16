//! The `docx` family (ADR-0032 P4): the contract, the selected-tools rule,
//! every tool end to end on real packages through an in-memory host, and the
//! rules every write must keep (all or nothing, checked on the saved bytes at
//! the target, one sentence per cause, names that round-trip, no overwrite
//! unless asked, one parse and one save per edit).

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use elitea_office_core::testing::{counters, reset_counters};
use elitea_office_core::{DocFacts, Document};
use serde_json::{Value, json};

use super::families::artifact::ArtifactToolAuthority;
use super::families::docx::config::DocxToolkitConfig;
use super::families::docx::ops;
use super::families::docx::schema::{ALL_TOOLS, DocxTool};
use super::families::docx::tools::{build_docx_toolset, build_with_host};
use super::families::office_common::OfficeConfigErrorCode;
use super::families::office_host::{OfficeFailure, SharedOfficeToolHost};
use super::materialize::materialize_configured_toolsets_with_artifact_authority;
use super::office_test_support::{
    FailHost, FakeHost, NoPlatform, TINY_PNG, context, policy, settings, test_limits,
};
use super::sdk_conformance;
use super::snapshot::FrozenToolSnapshot;
use crate::host::PlatformWriter;

async fn tools_for(host: &Arc<FakeHost>, selected: Option<Value>) -> Vec<Arc<dyn Tool>> {
    let config = DocxToolkitConfig::parse(&settings(selected)).expect("config");
    let shared: SharedOfficeToolHost = Arc::clone(host) as SharedOfficeToolHost;
    let toolset =
        build_with_host("doc", &config, &policy(), &shared, test_limits()).expect("toolset");
    toolset
        .tools(context() as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools")
}

struct Rig {
    host: Arc<FakeHost>,
    tools: Vec<Arc<dyn Tool>>,
}

impl Rig {
    async fn new() -> Self {
        let host = Arc::new(FakeHost::default());
        let tools = tools_for(&host, None).await;
        Self { host, tools }
    }

    async fn call(&self, tool: &str, args: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|t| t.name() == tool)
            .unwrap_or_else(|| panic!("no tool {tool}"));
        tool.execute(context() as Arc<dyn ToolContext>, args)
            .await
            .expect("a failure is a result, not an error")
    }

    async fn text(&self, tool: &str, args: Value) -> String {
        match self.call(tool, args).await {
            Value::String(s) => s,
            other => panic!("expected a sentence, got {other}"),
        }
    }

    /// The facts of a stored document.
    fn facts(&self, name: &str) -> DocFacts {
        elitea_office_core::facts_of_docx_package(&self.host.get(name), &test_limits())
            .expect("a valid document")
    }

    fn writes(&self) -> usize {
        self.host.writes.lock().expect("writes").len()
    }
}

/// A document with a heading, two paragraphs, a table and placeholders.
fn template() -> Vec<u8> {
    let l = test_limits();
    let mut d = Document::new(&l);
    d.add_heading("Report {{title}}", 1).unwrap();
    d.add_paragraph("Hello {{name}}, welcome.").unwrap();
    d.add_paragraph("Static text").unwrap();
    let t = d.add_table(2, 2).unwrap();
    d.set_table_cell(t, 0, 0, "Region").unwrap();
    d.set_table_cell(t, 1, 1, "[PLACEHOLDER]").unwrap();
    // Reopen so the document is an opened one, as every tool call sees it.
    Document::open(&d.save().unwrap(), &l)
        .unwrap()
        .save()
        .unwrap()
}

fn paragraphs(facts: &DocFacts) -> Vec<String> {
    facts.paragraphs().iter().map(|p| p.text.clone()).collect()
}

// -- The contract ---------------------------------------------------------

#[tokio::test]
async fn the_family_serves_its_13_tools() {
    let rig = Rig::new().await;
    let names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
    assert_eq!(
        names,
        [
            "create_document",
            "read_document",
            "add_heading",
            "add_paragraph",
            "set_paragraph_text",
            "add_table",
            "set_table_cell",
            "add_image",
            "add_chart",
            "list_placeholders",
            "replace_placeholders",
            "export_pdf",
            "render_page",
        ]
    );
    for tool in &rig.tools {
        assert!(
            tool.description().starts_with("Toolkit: doc\n"),
            "{}",
            tool.name()
        );
        assert!(tool.description().chars().count() <= 1_000);
        let schema = tool.parameters_schema().expect("a schema");
        assert_eq!(schema["type"], "object", "{}", tool.name());
        assert_eq!(schema["additionalProperties"], false, "{}", tool.name());
        let props = schema["properties"].as_object().expect("properties");
        for required in schema["required"].as_array().expect("required") {
            assert!(
                props.contains_key(required.as_str().unwrap()),
                "{}",
                tool.name()
            );
        }
    }
    let by_name = |n: &str| rig.tools.iter().find(|t| t.name() == n).expect("tool");
    assert!(by_name("read_document").is_read_only());
    assert!(by_name("list_placeholders").is_read_only());
    assert!(!by_name("replace_placeholders").is_read_only());
    assert!(!by_name("export_pdf").is_read_only());
}

/// The SDK has no `docx` toolkit. The day it grows one, its names and schemas
/// are the contract (saved agents are written against them), so this fails and
/// the family is reconciled with them, as `pptx` is.
#[test]
fn there_is_no_sdk_docx_toolkit_to_stay_callable_by() {
    assert!(!sdk_conformance::has_type("docx"));
}

#[tokio::test]
async fn the_selected_tools_rule_follows_the_artifact_family() {
    let host = Arc::new(FakeHost::default());
    for (selected, want) in [
        (None, 13),
        (Some(json!([])), 13),
        (Some(Value::Null), 13),
        (Some(json!(["add_heading", "read_document", "nope"])), 2),
        (Some(json!(["nope"])), 0),
        (Some(json!(["add_heading", "add_heading"])), 1),
    ] {
        let tools = tools_for(&host, selected.clone()).await;
        assert_eq!(tools.len(), want, "{selected:?}");
    }
    for bad in [
        json!({}),
        json!({"bucket_name": "Bad_Name"}),
        json!({"bucket_name": "b", "selected_tools": "x"}),
        json!({"bucket_name": "bucket", "selected_tools": [1]}),
    ] {
        assert!(DocxToolkitConfig::parse(bad.as_object().unwrap()).is_err());
    }
    let long = "a".repeat(64);
    let err = DocxToolkitConfig::parse(json!({"bucket_name": long}).as_object().unwrap())
        .err()
        .expect("too long");
    assert_eq!(err.code(), OfficeConfigErrorCode::ResourceExhausted);
}

#[test]
fn the_ui_catalogue_lists_the_docx_type_and_every_tool() {
    let entry = super::families::docx::schema::type_entry();
    let tools = entry["args_schemas"].as_object().unwrap();
    assert_eq!(tools.len(), ALL_TOOLS.len());
    for tool in ALL_TOOLS {
        let schema = &tools[tool.name()];
        assert_eq!(schema["type"], "object");
        assert!(schema["title"].as_str().unwrap().ends_with("Args"));
        assert!(schema.get("additionalProperties").is_none());
    }
    let settings = &entry["settings"];
    assert_eq!(settings["required"], json!(["bucket_name"]));
    let listed: Vec<&str> = settings["properties"]["selected_tools"]["items"]["enum"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    assert_eq!(listed.len(), ALL_TOOLS.len());
}

// -- create ---------------------------------------------------------------

#[tokio::test]
async fn create_document_makes_a_titled_document_and_never_replaces_unasked() {
    let rig = Rig::new().await;
    let said = rig
        .text(
            "create_document",
            json!({"file_name": "docs/a.docx", "title": "Hello"}),
        )
        .await;
    assert!(said.contains("`docs/a.docx` in bucket `bucket`"), "{said}");
    let facts = rig.facts("docs/a.docx");
    assert_eq!(paragraphs(&facts), ["Hello"]);
    assert_eq!(facts.paragraphs()[0].style.as_deref(), Some("Heading1"));
    // Created with overwrite=false: the server's 409 is "already exists".
    let before = rig.host.get("docs/a.docx");
    let said = rig
        .text(
            "create_document",
            json!({"file_name": "docs/a.docx", "title": "Other"}),
        )
        .await;
    assert!(said.contains("already exists"), "{said}");
    assert!(said.contains("overwrite=true"), "{said}");
    assert_eq!(rig.host.get("docs/a.docx"), before);
    assert_eq!(
        rig.host.writes.lock().unwrap().as_slice(),
        [
            ("docs/a.docx".to_owned(), false),
            ("docs/a.docx".to_owned(), false)
        ]
    );
    // Asked to replace, it does.
    rig.text(
        "create_document",
        json!({"file_name": "docs/a.docx", "title": "Other", "overwrite": true}),
    )
    .await;
    assert_eq!(paragraphs(&rig.facts("docs/a.docx")), ["Other"]);
}

#[tokio::test]
async fn an_empty_document_is_valid() {
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "empty.docx"}))
        .await;
    assert!(rig.facts("empty.docx").paragraphs().is_empty());
    let md = rig
        .text("read_document", json!({"file_name": "empty.docx"}))
        .await;
    assert!(!md.contains("rror"), "{md}");
}

// -- authoring --------------------------------------------------------------

#[tokio::test]
async fn authoring_tools_edit_in_place_and_report_numbers_that_chain() {
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "d.docx"}))
        .await;
    let said = rig
        .text(
            "add_heading",
            json!({"file_name": "d.docx", "text": "Intro", "level": 2}),
        )
        .await;
    assert!(said.contains("paragraph 1"), "{said}");
    let said = rig
        .text(
            "add_paragraph",
            json!({"file_name": "d.docx", "text": "First"}),
        )
        .await;
    assert!(said.contains("paragraph 2"), "{said}");
    let said = rig
        .text(
            "set_paragraph_text",
            json!({"file_name": "d.docx", "paragraph": 2, "text": "Changed"}),
        )
        .await;
    assert!(said.contains("paragraph 2"), "{said}");
    let said = rig
        .text(
            "add_table",
            json!({"file_name": "d.docx", "rows": [["A", "B"], ["1", "2"]]}),
        )
        .await;
    assert!(said.contains("table 1"), "{said}");
    rig.text(
        "set_table_cell",
        json!({"file_name": "d.docx", "table": 1, "row": 2, "column": 2, "text": "two"}),
    )
    .await;
    let facts = rig.facts("d.docx");
    assert_eq!(paragraphs(&facts), ["Intro", "Changed"]);
    assert_eq!(facts.paragraphs()[0].style.as_deref(), Some("Heading2"));
    assert_eq!(
        facts.tables()[0].rows,
        vec![vec!["A", "B"], vec!["1", "two"]]
    );
    // The table is visible: ruled, with a styled header.
    let xml = part(&rig.host.get("d.docx"), "word/document.xml");
    assert!(xml.contains("w:tblBorders"), "{xml}");
    assert!(xml.contains("w:shd"), "{xml}");
}

#[tokio::test]
async fn output_file_name_leaves_the_source_alone() {
    let rig = Rig::new().await;
    rig.host.put("src.docx", template());
    let before = rig.host.get("src.docx");
    rig.text(
        "add_paragraph",
        json!({"file_name": "src.docx", "text": "More", "output_file_name": "out/copy.docx"}),
    )
    .await;
    assert_eq!(rig.host.get("src.docx"), before);
    assert_eq!(rig.facts("out/copy.docx").paragraphs().len(), 4);
}

#[tokio::test]
async fn line_breaks_are_break_runs_in_every_text_a_tool_writes() {
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "d.docx"}))
        .await;
    rig.text(
        "add_paragraph",
        json!({"file_name": "d.docx", "text": "line one\nline two"}),
    )
    .await;
    rig.text(
        "add_table",
        json!({"file_name": "d.docx", "rows": [["a\nb"]]}),
    )
    .await;
    let xml = part(&rig.host.get("d.docx"), "word/document.xml");
    assert!(xml.matches("<w:br/>").count() >= 2, "{xml}");
    for run in xml.split("<w:t>").skip(1).chain(xml.split("<w:t ").skip(1)) {
        let text = run.split("</w:t>").next().unwrap();
        assert!(!text.contains('\n'), "a raw newline in a run: {text:?}");
    }
    let facts = rig.facts("d.docx");
    assert_eq!(facts.paragraphs()[0].text, "line one\nline two");
    assert_eq!(facts.tables()[0].rows[0][0], "a\nb");
    // A replacement with a line break too.
    rig.host.put("t.docx", template());
    rig.call(
        "replace_placeholders",
        json!({"file_name": "t.docx", "replacements": {"{{name}}": "Ada\nLovelace"}}),
    )
    .await;
    assert_eq!(
        rig.facts("t.docx").paragraphs()[1].text,
        "Hello Ada\nLovelace, welcome."
    );
}

#[tokio::test]
async fn a_control_character_is_refused_before_anything_is_written() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    let before = rig.host.get("t.docx");
    let said = rig
        .text(
            "add_paragraph",
            json!({"file_name": "t.docx", "text": "bad \u{1} text"}),
        )
        .await;
    assert!(said.contains("control character"), "{said}");
    assert_eq!(rig.host.get("t.docx"), before);
    assert_eq!(rig.writes(), 0);
}

#[tokio::test]
async fn every_refusal_happens_before_anything_is_written() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    rig.host.put("notimage.png", b"not an image".to_vec());
    let before = rig.host.get("t.docx");
    let cases = [
        (
            "set_paragraph_text",
            json!({"file_name": "t.docx", "paragraph": 9, "text": "x"}),
            "Paragraph 9 does not exist",
        ),
        (
            "set_paragraph_text",
            json!({"file_name": "t.docx", "paragraph": 0, "text": "x"}),
            "does not exist",
        ),
        (
            "set_table_cell",
            json!({"file_name": "t.docx", "row": 3, "column": 1, "text": "x"}),
            "Row 3 does not exist",
        ),
        (
            "set_table_cell",
            json!({"file_name": "t.docx", "table": 2, "row": 1, "column": 1, "text": "x"}),
            "Table 2 does not exist",
        ),
        (
            "add_table",
            json!({"file_name": "t.docx", "rows": [["a", "b"], ["c"]]}),
            "same number of cells",
        ),
        (
            "add_table",
            json!({"file_name": "t.docx", "rows": []}),
            "at least one row",
        ),
        (
            "add_heading",
            json!({"file_name": "t.docx", "text": "x", "level": 12}),
            "1 to 9",
        ),
        (
            "add_image",
            json!({"file_name": "t.docx", "image_file_name": "notimage.png"}),
            "not a PNG, JPEG or GIF",
        ),
        (
            "add_image",
            json!({"file_name": "t.docx", "image_file_name": "missing.png"}),
            "no such file",
        ),
        (
            "add_chart",
            json!({"file_name": "t.docx", "categories": ["a", "b"], "series": [{"name": "s", "values": [1]}]}),
            "exactly one per category",
        ),
        (
            "add_chart",
            json!({"file_name": "t.docx", "chart_type": "doughnut", "categories": ["a"], "series": [{"name": "s", "values": [1]}]}),
            "not available here",
        ),
        (
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": {"abc": "1", "bcd": "2"}}),
            "overlap",
        ),
        (
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": {"a\nb": "1"}}),
            "line break or a tab",
        ),
        (
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": {}}),
            "at least one",
        ),
        (
            "set_paragraph_text",
            json!({"file_name": "../x.docx", "paragraph": 1, "text": "x"}),
            "`..`",
        ),
    ];
    for (tool, args, want) in cases {
        let said = rig.text(tool, args.clone()).await;
        assert!(said.contains(want), "{tool}: {said}");
    }
    let many: serde_json::Map<String, Value> = (0..101)
        .map(|i| (format!("k{i}"), Value::String("v".into())))
        .collect();
    let said = rig
        .text(
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": many}),
        )
        .await;
    assert!(said.contains("Too many replacements"), "{said}");
    assert_eq!(rig.host.get("t.docx"), before);
    assert_eq!(rig.writes(), 0, "a refused call wrote a file");
}

#[tokio::test]
async fn a_paragraph_with_a_picture_is_not_rewritten() {
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "d.docx"}))
        .await;
    rig.host.put("pic.png", TINY_PNG.to_vec());
    rig.text("add_paragraph", json!({"file_name": "d.docx", "text": "x"}))
        .await;
    let said = rig
        .text(
            "add_image",
            json!({"file_name": "d.docx", "image_file_name": "pic.png", "width": 2}),
        )
        .await;
    assert!(said.contains("Added a picture"), "{said}");
    let facts = rig.facts("d.docx");
    assert_eq!(facts.pictures, 1);
    let at = facts
        .paragraphs()
        .iter()
        .position(|p| p.pictures == 1)
        .expect("the picture is in a paragraph")
        + 1;
    let before = rig.host.get("d.docx");
    let said = rig
        .text(
            "set_paragraph_text",
            json!({"file_name": "d.docx", "paragraph": at, "text": "gone"}),
        )
        .await;
    assert!(said.contains("holds a picture or a chart"), "{said}");
    assert_eq!(rig.host.get("d.docx"), before);
}

#[tokio::test]
async fn an_image_keeps_its_proportions() {
    let tall = {
        // A 1x2 PNG header is enough for the size probe; the library keeps
        // the bytes as they are.
        let mut v = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
        v.extend_from_slice(&1_u32.to_be_bytes());
        v.extend_from_slice(&2_u32.to_be_bytes());
        v.extend_from_slice(&[8, 2, 0, 0, 0]);
        v
    };
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "d.docx"}))
        .await;
    rig.host.put("tall.png", tall);
    // Width only: the height follows (1:2).
    rig.text(
        "add_image",
        json!({"file_name": "d.docx", "image_file_name": "tall.png", "width": 2}),
    )
    .await;
    let xml = part(&rig.host.get("d.docx"), "word/document.xml");
    assert!(xml.contains(&format!("cx=\"{}\"", 2 * 914_400)), "{xml}");
    assert!(xml.contains(&format!("cy=\"{}\"", 4 * 914_400)), "{xml}");
}

#[tokio::test]
async fn add_chart_puts_a_native_chart_at_the_end() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    let said = rig
        .text(
            "add_chart",
            json!({
                "file_name": "t.docx",
                "chart_type": "line",
                "categories": ["Q1", "Q2"],
                "series": [{"name": "Sales", "values": [1.0, 2.5]}],
                "title": "Sales"
            }),
        )
        .await;
    assert!(said.contains("Added a chart"), "{said}");
    let facts = rig.facts("t.docx");
    assert_eq!(facts.charts, 1);
    assert!(facts.parts.keys().any(|k| k.starts_with("word/charts")));
}

// -- placeholders -----------------------------------------------------------

#[tokio::test]
async fn list_placeholders_finds_paragraphs_and_cells_and_numbers_them() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    let listing = rig
        .call("list_placeholders", json!({"file_name": "t.docx"}))
        .await;
    assert_eq!(listing["total_locations"], 3);
    let rows = listing["placeholders"].as_array().unwrap();
    assert_eq!(rows[0]["paragraph"], 1);
    assert_eq!(rows[0]["style"], "Heading1");
    assert_eq!(rows[1]["text"], "Hello {{name}}, welcome.");
    assert_eq!(rows[2]["location"], "table_cell");
    assert_eq!(
        (rows[2]["row"].clone(), rows[2]["column"].clone()),
        (json!(2), json!(2))
    );
    let all = rig
        .call(
            "list_placeholders",
            json!({"file_name": "t.docx", "include_all": true}),
        )
        .await;
    assert_eq!(all["total_locations"], 5);
    // Reading changes nothing.
    assert_eq!(rig.writes(), 0);
}

#[tokio::test]
async fn replace_placeholders_is_one_pass_leftmost_longest_over_the_original() {
    let rig = Rig::new().await;
    let l = test_limits();
    let mut d = Document::new(&l);
    d.add_paragraph("a {{x}} and {{xy}} and {{y}}").unwrap();
    d.add_paragraph("{{x}}{{y}}").unwrap();
    let t = d.add_table(1, 1).unwrap();
    d.set_table_cell(t, 0, 0, "cell {{x}}").unwrap();
    rig.host.put("t.docx", d.save().unwrap());
    let result = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": {
                "{{x}}": "{{y}}", "{{y}}": "[Y]", "{{xy}}": "XY", "{{unused}}": "z"
            }}),
        )
        .await;
    assert_eq!(result["status"], "success");
    assert_eq!(result["replacements_applied"], 6);
    assert_eq!(result["keys_not_found"], json!(["{{unused}}"]));
    let facts = rig.facts("t.docx");
    // `{{x}}` became the text `{{y}}`, which was not replaced again.
    assert_eq!(paragraphs(&facts), ["a {{y}} and XY and [Y]", "{{y}}[Y]"]);
    assert_eq!(facts.tables()[0].rows[0][0], "cell {{y}}");
}

#[tokio::test]
async fn a_replacement_that_contains_its_key_terminates() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    let result = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "t.docx", "replacements": {"Static": "Static Static"}}),
        )
        .await;
    assert_eq!(result["replacements_applied"], 1);
    assert_eq!(paragraphs(&rig.facts("t.docx"))[2], "Static Static text");
}

#[tokio::test]
async fn replacements_are_checked_at_every_matched_place_not_document_wide() {
    // The saved bytes are judged paragraph by paragraph and cell by cell: the
    // same words somewhere else prove nothing.
    let before = facts_of(&["one", "two"], &[]);
    let plan = ops::Plan::SetParagraph {
        index: 0,
        text: "X".into(),
    };
    // After: paragraph 1 unchanged, "X" appears as paragraph 2 instead.
    let after = facts_of(&["one", "X"], &[]);
    let said = ops::verify(&before, &after, &plan).expect_err("wrong place");
    assert!(said.contains("Nothing was saved"), "{said}");
    assert!(said.contains("paragraph"), "{said}");
    // In the right place it passes.
    ops::verify(&before, &facts_of(&["X", "two"], &[]), &plan).expect("right place");
    // A cell edit is judged at its row and column.
    let before = facts_of(&["p"], &[vec![vec!["a", "b"], vec!["c", "d"]]]);
    let plan = ops::Plan::Cell {
        table: 0,
        row: 1,
        col: 1,
        text: "Z".into(),
    };
    let wrong = facts_of(&["p"], &[vec![vec!["a", "Z"], vec!["c", "d"]]]);
    assert!(ops::verify(&before, &wrong, &plan).is_err());
    let right = facts_of(&["p"], &[vec![vec!["a", "b"], vec!["c", "Z"]]]);
    ops::verify(&before, &right, &plan).expect("right cell");
    // An append is judged at the end, with the old content as it was.
    let plan = ops::Plan::Paragraph { text: "new".into() };
    assert!(ops::verify(&facts_of(&["a"], &[]), &facts_of(&["new", "a"], &[]), &plan).is_err());
    ops::verify(&facts_of(&["a"], &[]), &facts_of(&["a", "new"], &[]), &plan).expect("appended");
}

fn facts_of(paragraphs: &[&str], tables: &[Vec<Vec<&str>>]) -> DocFacts {
    use elitea_office_core::{DocBlock, DocParagraph, DocTable};
    let mut blocks: Vec<DocBlock> = paragraphs
        .iter()
        .map(|p| {
            DocBlock::Paragraph(DocParagraph {
                text: (*p).to_owned(),
                style: None,
                pictures: 0,
                charts: 0,
            })
        })
        .collect();
    for t in tables {
        blocks.push(DocBlock::Table(DocTable {
            rows: t
                .iter()
                .map(|r| r.iter().map(|c| (*c).to_owned()).collect())
                .collect(),
        }));
    }
    DocFacts {
        blocks,
        ..DocFacts::default()
    }
}

#[test]
fn a_save_that_loses_content_is_refused() {
    let mut before = facts_of(&["a"], &[]);
    before.pictures = 1;
    before.parts.insert("word/media/image".into(), 1);
    let mut after = facts_of(&["a", "b"], &[]);
    after.pictures = 0;
    let plan = ops::Plan::Paragraph { text: "b".into() };
    let said = ops::verify(&before, &after, &plan).expect_err("a picture was lost");
    assert!(said.contains("1 picture"), "{said}");
    after.pictures = 1;
    let said = ops::verify(&before, &after, &plan).expect_err("a part was lost");
    assert!(said.contains("word/media/image"), "{said}");
    after.parts.insert("word/media/image".into(), 1);
    ops::verify(&before, &after, &plan).expect("nothing lost");
}

#[tokio::test]
async fn replace_placeholders_reports_the_file_it_stored_in_a_form_the_next_call_accepts() {
    let rig = Rig::new().await;
    rig.host.put("deep/dir/t.docx", template());
    let result = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "/bucket/deep/dir/t.docx", "replacements": {"{{name}}": "Ada"},
                   "output_file_name": "deep/dir/out.docx"}),
        )
        .await;
    assert_eq!(result["file"], "deep/dir/out.docx");
    assert_eq!(result["bucket"], "bucket");
    // The reported key is the next call's file_name.
    let key = result["file"].as_str().unwrap();
    let said = rig
        .text(
            "add_paragraph",
            json!({"file_name": key, "text": "chained"}),
        )
        .await;
    assert!(
        said.contains("Saved `deep/dir/out.docx` in bucket `bucket`"),
        "{said}"
    );
    // And the platform's rooted path form works as input too.
    let md = rig
        .text(
            "read_document",
            json!({"file_name": "/bucket/deep/dir/out.docx"}),
        )
        .await;
    assert!(md.contains("Hello Ada"), "{md}");
}

// -- reads and exports ------------------------------------------------------

#[tokio::test]
async fn read_document_returns_markdown_or_text_and_cuts_long_ones() {
    let rig = Rig::new().await;
    rig.host.put("t.docx", template());
    let md = rig
        .text("read_document", json!({"file_name": "t.docx"}))
        .await;
    assert!(md.contains("# Report {{title}}"), "{md}");
    assert!(md.contains("Hello {{name}}"), "{md}");
    assert!(md.contains("Region"), "{md}");
    let plain = rig
        .text(
            "read_document",
            json!({"file_name": "t.docx", "format": "text"}),
        )
        .await;
    assert!(plain.contains("Static text"), "{plain}");
    assert!(!plain.contains('#'), "{plain}");
    // A long document is cut and says so.
    let l = test_limits();
    let mut d = Document::new(&l);
    for _ in 0..30 {
        d.add_paragraph(&"word ".repeat(1_500)).unwrap();
    }
    rig.host.put("long.docx", d.save().unwrap());
    let md = rig
        .text("read_document", json!({"file_name": "long.docx"}))
        .await;
    assert!(md.contains("[Truncated at 200000 characters"), "{md}");
    assert_eq!(rig.writes(), 0);
}

#[tokio::test]
async fn exports_and_renders_go_next_to_the_source_unless_named() {
    let rig = Rig::new().await;
    rig.host.put("reports/2026/t.docx", template());
    let said = rig
        .text("export_pdf", json!({"file_name": "reports/2026/t.docx"}))
        .await;
    assert!(said.contains("`reports/2026/t.pdf`"), "{said}");
    assert!(rig.host.get("reports/2026/t.pdf").starts_with(b"%PDF"));
    let result = rig
        .call("render_page", json!({"file_name": "reports/2026/t.docx"}))
        .await;
    assert_eq!(result["image"], "reports/2026/t-page-1.png");
    assert_eq!(result["bucket"], "bucket");
    assert!(
        rig.host
            .get("reports/2026/t-page-1.png")
            .starts_with(b"\x89PNG")
    );
    // Named outputs win.
    let said = rig
        .text(
            "export_pdf",
            json!({"file_name": "reports/2026/t.docx", "output_file_name": "pdf/x.pdf"}),
        )
        .await;
    assert!(said.contains("`pdf/x.pdf`"), "{said}");
    // A page past the end is a sentence about the page.
    let said = rig
        .text(
            "render_page",
            json!({"file_name": "reports/2026/t.docx", "page": 40}),
        )
        .await;
    assert!(said.contains("Page 40 does not exist"), "{said}");
    let said = rig
        .text(
            "render_page",
            json!({"file_name": "reports/2026/t.docx", "dpi": 5}),
        )
        .await;
    assert!(said.contains("dpi"), "{said}");
}

// -- host failures: one sentence per cause ---------------------------------

#[tokio::test]
async fn each_host_failure_has_its_own_sentence() {
    let mut sentences = std::collections::BTreeSet::new();
    for kind in [
        OfficeFailure::NotFound,
        OfficeFailure::AccessRefused,
        OfficeFailure::AlreadyExists,
        OfficeFailure::UnsupportedType,
        OfficeFailure::InvalidRequest,
        OfficeFailure::TooLarge,
        OfficeFailure::Unreachable,
    ] {
        let config = DocxToolkitConfig::parse(&settings(None)).unwrap();
        let host: SharedOfficeToolHost = Arc::new(FailHost(kind));
        let toolset = build_with_host("doc", &config, &policy(), &host, test_limits()).unwrap();
        let tools = toolset
            .tools(context() as Arc<dyn ReadonlyContext>)
            .await
            .unwrap();
        let tool = tools
            .iter()
            .find(|t| t.name() == "read_document")
            .expect("tool");
        let said = tool
            .execute(
                context() as Arc<dyn ToolContext>,
                json!({"file_name": "x.docx"}),
            )
            .await
            .unwrap();
        let said = said.as_str().expect("a sentence").to_owned();
        assert!(
            said.starts_with("Could not read `x.docx`"),
            "{kind:?}: {said}"
        );
        assert!(!said.contains("list_files"), "{said}");
        assert!(
            sentences.insert(said.clone()),
            "{kind:?} repeats a sentence: {said}"
        );
    }
    assert_eq!(sentences.len(), 7);
    // The unsupported-type sentence names what a docx toolkit expects.
    let unsupported = sentences
        .iter()
        .find(|s| s.contains("does not accept that type"))
        .expect("sentence");
    assert!(unsupported.contains(".docx"), "{unsupported}");
    // A write to a taken name that did not ask to replace it.
    let config = DocxToolkitConfig::parse(&settings(None)).unwrap();
    let host: SharedOfficeToolHost = Arc::new(FailHost(OfficeFailure::AlreadyExists));
    let toolset = build_with_host("doc", &config, &policy(), &host, test_limits()).unwrap();
    let tools = toolset
        .tools(context() as Arc<dyn ReadonlyContext>)
        .await
        .unwrap();
    let create = tools
        .iter()
        .find(|t| t.name() == "create_document")
        .unwrap();
    let said = create
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({"file_name": "x.docx"}),
        )
        .await
        .unwrap();
    assert!(said.as_str().unwrap().contains("already exists"), "{said}");
}

// -- one parse, one save ----------------------------------------------------

#[test]
fn an_edit_opens_once_and_serialises_once() {
    let l = test_limits();
    let bytes = template();
    for (tool, args) in [
        (DocxTool::AddHeading, json!({"text": "H", "level": 2})),
        (DocxTool::AddParagraph, json!({"text": "P\nQ"})),
        (
            DocxTool::SetParagraphText,
            json!({"paragraph": 2, "text": "S"}),
        ),
        (
            DocxTool::AddTable,
            json!({"rows": [["a", "b"], ["c", "d"]]}),
        ),
        (
            DocxTool::SetTableCell,
            json!({"row": 1, "column": 1, "text": "T"}),
        ),
        (
            DocxTool::ReplacePlaceholders,
            json!({"replacements": {"{{name}}": "Ada", "{{title}}": "T"}}),
        ),
        (
            DocxTool::AddChart,
            json!({"categories": ["a"], "series": [{"name": "s", "values": [1]}]}),
        ),
    ] {
        reset_counters();
        ops::run_edit(tool, &args, &bytes, None, &l).unwrap_or_else(|e| panic!("{tool:?}: {e}"));
        let (opens, saves, reads) = counters();
        assert_eq!(opens, 1, "{tool:?}: library parses");
        assert_eq!(saves, 1, "{tool:?}: serialisations");
        assert_eq!(reads, 2, "{tool:?}: package reads (before and after)");
    }
    reset_counters();
    ops::run_edit(
        DocxTool::AddImage,
        &json!({"width": 1}),
        &bytes,
        Some(TINY_PNG),
        &l,
    )
    .unwrap();
    assert_eq!((counters().0, counters().1), (1, 1));
    // Listing placeholders parses nothing with the library.
    reset_counters();
    ops::list_report(&ops::facts(&bytes, &l).unwrap(), false);
    assert_eq!((counters().0, counters().1), (0, 0));
}

// -- materialization, the Office host and pipelines ----------------------------

fn docx_version() -> Value {
    json!({"tools": [{
        "id": 7,
        "type": "docx",
        "toolkit_name": "doc",
        "settings": {"bucket_name": "bucket", "selected_tools": ["read_document", "add_heading"]}
    }]})
}

#[tokio::test]
async fn materialization_needs_the_office_capability_on_the_artifact_authority() {
    let host = Arc::new(FakeHost::default());
    let platform: Arc<dyn PlatformWriter> = Arc::new(NoPlatform);
    let version = docx_version();
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());

    // Office lent: the toolkit materializes with its selected tools.
    let lent = ArtifactToolAuthority::new(Arc::clone(&platform))
        .with_office(Arc::clone(&host) as SharedOfficeToolHost);
    let (toolsets, _) = materialize_configured_toolsets_with_artifact_authority(
        &snapshot,
        &policy(),
        &serde_json::Map::new(),
        Some(&lent),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let mut names = Vec::new();
    for tool in toolsets[0]
        .tools(context() as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools")
    {
        names.push(tool.name().to_owned());
    }
    names.sort();
    assert_eq!(names, ["add_heading", "read_document"]);

    // Artifact authority without Office, and no authority at all: skipped as
    // unsupported, never bound to tools that would fail on every call.
    let bare = ArtifactToolAuthority::new(platform);
    let config = DocxToolkitConfig::parse(&settings(None)).expect("config");
    assert!(
        build_docx_toolset("doc", &config, &policy(), &bare)
            .expect("builds")
            .is_none()
    );
    for authority in [Some(&bare), None] {
        let (toolsets, _) = materialize_configured_toolsets_with_artifact_authority(
            &snapshot,
            &policy(),
            &serde_json::Map::new(),
            authority,
        )
        .await
        .expect("an unserved toolkit is skipped, not fatal");
        assert!(toolsets.is_empty(), "skipped, not bound");
    }
}

// -- helpers ------------------------------------------------------------------

fn part(package: &[u8], name: &str) -> String {
    elitea_office_core::testing::part_text(package, name).expect("the part exists")
}

#[tokio::test]
async fn a_null_table_cell_is_an_empty_cell_and_other_scalars_are_their_text() {
    let rig = Rig::new().await;
    rig.text("create_document", json!({"file_name": "d.docx"}))
        .await;
    let said = rig
        .text(
            "add_table",
            json!({"file_name": "d.docx", "rows": [["a", null, 7], [true, 1.5, "z"]]}),
        )
        .await;
    assert!(said.contains("table 1"), "{said}");
    assert_eq!(
        rig.facts("d.docx").tables()[0].rows,
        vec![vec!["a", "", "7"], vec!["true", "1.5", "z"]]
    );
}

/// Live 4.45.0 (D2): one Office toolkit saved without `bucket_name` failed
/// EVERY turn of the agent (`native_agent.invalid_configuration`). It is now
/// left out alone, with the reason in the run's notice, and the agent's
/// other toolkits are served; the selected tools it named and the family
/// does not serve are reported too (D3).
#[tokio::test]
async fn an_office_toolkit_without_a_bucket_is_left_out_not_fatal() {
    use super::families::index_tools::{IndexToolSupport, IndexToolsWithheld};
    use super::materialize::{
        LeftOutReason, left_out_toolkits_notice_text,
        materialize_configured_toolsets_reporting_refusals,
    };
    let host = Arc::new(FakeHost::default());
    let platform: Arc<dyn PlatformWriter> = Arc::new(NoPlatform);
    let lent =
        ArtifactToolAuthority::new(platform).with_office(Arc::clone(&host) as SharedOfficeToolHost);
    let version = json!({"tools": [
        {"id": 1, "type": "docx", "toolkit_name": "nobucket",
         "settings": {"selected_tools": ["read_document", "not_a_docx_tool"]}},
        {"id": 2, "type": "xlsx", "toolkit_name": "blank",
         "settings": {"bucket_name": "  "}},
        {"id": 3, "type": "pptx", "toolkit_name": "deck",
         "settings": {"bucket_name": "bucket", "selected_tools": ["to_markdown", "not_a_pptx_tool"]}},
        {"id": 4, "type": "artifact", "toolkit_name": "files", "settings": {}}
    ]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let materialized = materialize_configured_toolsets_reporting_refusals(
        &snapshot,
        &policy(),
        &serde_json::Map::new(),
        Some(&lent),
        IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
    )
    .await
    .expect("a toolkit without a bucket never fails the run");
    assert_eq!(
        materialized.toolsets.len(),
        1,
        "only the pptx toolkit binds"
    );
    let refused: Vec<_> = materialized
        .left_out
        .iter()
        .filter(|toolkit| toolkit.cause() == LeftOutReason::UnsupportedSetting)
        .map(|toolkit| toolkit.toolkit_name().to_owned())
        .collect();
    assert_eq!(refused, ["nobucket", "blank", "files"]);
    // Review C3: a REFUSED toolkit is named once, at toolkit level; the
    // tools it selected (served or not) get no per-tool lines of their own.
    assert!(
        materialized
            .left_out
            .iter()
            .filter(|toolkit| toolkit.toolkit_name() == "nobucket")
            .all(|toolkit| toolkit.tool_name().is_none()),
        "{:?}",
        materialized.left_out
    );
    let notice = left_out_toolkits_notice_text(&materialized.left_out).expect("a notice");
    assert!(
        notice.contains("toolkit 'nobucket' (docx) is not enabled on this worker: the toolkit has no usable bucket"),
        "{notice}"
    );
    assert!(
        notice.contains("tool 'deck.not_a_pptx_tool' (pptx) is not enabled on this worker"),
        "{notice}"
    );
}

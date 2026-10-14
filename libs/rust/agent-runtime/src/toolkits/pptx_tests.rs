//! The `pptx` family (ADR-0032 P3): the SDK schema gate, the selected-tools
//! rule, materialization, every tool end to end on real packages through an
//! in-memory host, the two model-calling tools against a scripted model, and
//! the prompt and reply parsing.

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use elitea_office_core::{Bullet, Deck, Rect, SlideLayout};
use serde_json::{Map, Value, json};

use super::families::artifact::ArtifactToolAuthority;
use super::families::office_host::{OfficeFailure, OfficeHostError, SharedOfficeToolHost};
use super::families::pptx::config::{PptxConfigErrorCode, PptxToolkitConfig};
use super::families::pptx::model;
use super::families::pptx::ops;
use super::families::pptx::schema::{ALL_TOOLS, PptxTool};
use super::families::pptx::tools::{build_pptx_toolset, build_with_host, host_sentence};
use super::materialize::materialize_configured_toolsets_with_artifact_authority;
use super::office_test_support::{
    FakeHost, NoPlatform, TINY_PNG, context, policy, settings, test_limits,
};
use super::sdk_conformance;
use super::snapshot::FrozenToolSnapshot;
use crate::host::PlatformWriter;

async fn tools_for(host: &Arc<FakeHost>, selected: Option<Value>) -> Vec<Arc<dyn Tool>> {
    let config = PptxToolkitConfig::parse(&settings(selected)).expect("config");
    let shared: SharedOfficeToolHost = Arc::clone(host) as SharedOfficeToolHost;
    let toolset =
        build_with_host("deck", &config, &policy(), &shared, test_limits()).expect("toolset");
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

    fn markdown(&self, name: &str) -> String {
        Deck::open(&self.host.get(name), &test_limits())
            .expect("a valid deck")
            .to_markdown()
            .expect("markdown")
    }
}

/// A template with placeholders in a title, a text box and a table cell.
fn template() -> Vec<u8> {
    let l = test_limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s0, "{{title}}").unwrap();
    d.add_text(
        s0,
        "Hello {{name}}, welcome",
        Rect::inches(1.0, 2.0, 6.0, 1.0),
    )
    .unwrap();
    let s1 = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s1, "Static text", Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let t = d
        .add_table(s1, 2, 2, Rect::inches(1.0, 2.0, 4.0, 1.0))
        .unwrap();
    d.set_table_cell(s1, t, 0, 0, "Region").unwrap();
    d.set_table_cell(s1, t, 1, 1, "[PLACEHOLDER]").unwrap();
    // Reopen so the deck is an opened one, as every tool call sees it.
    Deck::open(&d.save().unwrap(), &l).unwrap().save().unwrap()
}

// -- The contract ---------------------------------------------------------

#[tokio::test]
async fn the_family_serves_its_21_tools_and_keeps_the_sdk_contract() {
    let rig = Rig::new().await;
    let names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
    assert_eq!(names.len(), 21);
    assert_eq!(&names[..2], ["fill_template", "translate_presentation"]);
    for expected in [
        "list_placeholders",
        "replace_placeholders",
        "create_presentation",
        "add_slide",
        "delete_slide",
        "move_slide",
        "set_title",
        "set_text",
        "add_bullets",
        "add_table",
        "set_table_cell",
        "add_chart",
        "add_image",
        "set_notes",
        "apply_theme",
        "render_preview",
        "design_check",
        "to_markdown",
        "export_pdf",
    ] {
        assert!(names.contains(&expected), "{expected}");
    }
    // Only the two SDK names are in the SDK snapshot; the rest are this
    // family's own and exempted by name, with a reason, in the exemptions.
    sdk_conformance::assert_sdk_conformance("pptx", &rig.tools);
    for tool in &rig.tools {
        assert!(
            tool.description().starts_with("Toolkit: deck\n"),
            "{}",
            tool.name()
        );
        assert!(tool.description().chars().count() <= 1_000);
    }
    let by_name = |n: &str| rig.tools.iter().find(|t| t.name() == n).expect("tool");
    assert!(by_name("to_markdown").is_read_only());
    assert!(by_name("list_placeholders").is_read_only());
    assert!(by_name("design_check").is_read_only());
    assert!(!by_name("fill_template").is_read_only());
    assert!(!by_name("render_preview").is_read_only());
    // Descriptions of the SDK tools are the SDK's.
    assert!(
        by_name("fill_template")
            .description()
            .contains("Fill a PPTX template with content based on the provided description.")
    );
    assert!(
        by_name("translate_presentation")
            .description()
            .ends_with("Translate text in a PowerPoint presentation to another language")
    );
}

#[test]
fn the_sdk_tools_keep_their_argument_schemas() {
    let fill = PptxTool::FillTemplate.parameters_schema();
    let props = fill["properties"].as_object().expect("properties");
    let mut names: Vec<&str> = props.keys().map(String::as_str).collect();
    names.sort_unstable();
    assert_eq!(
        names,
        [
            "batch_size",
            "content_description",
            "file_name",
            "output_file_name",
            "pdf_file_name"
        ]
    );
    assert_eq!(
        fill["required"],
        json!(["file_name", "output_file_name", "content_description"])
    );
    let tr = PptxTool::TranslatePresentation.parameters_schema();
    assert_eq!(
        tr["required"],
        json!(["file_name", "output_file_name", "target_language"])
    );
    // Every schema is a closed object that names its required fields.
    for tool in ALL_TOOLS {
        let s = tool.parameters_schema();
        assert_eq!(s["type"], "object", "{}", tool.name());
        assert_eq!(s["additionalProperties"], false, "{}", tool.name());
        for r in s["required"].as_array().expect("required") {
            assert!(s["properties"][r.as_str().expect("name")].is_object());
        }
        assert_eq!(PptxTool::parse(tool.name()), Some(tool));
    }
    assert_eq!(PptxTool::parse("index_data"), None);
}

#[tokio::test]
async fn the_selected_tools_rule_follows_the_artifact_family() {
    let host = Arc::new(FakeHost::default());
    let names = |tools: &[Arc<dyn Tool>]| -> Vec<String> {
        tools.iter().map(|t| t.name().to_owned()).collect()
    };
    assert_eq!(names(&tools_for(&host, None).await).len(), 21);
    assert_eq!(names(&tools_for(&host, Some(json!([]))).await).len(), 21);
    assert_eq!(names(&tools_for(&host, Some(Value::Null)).await).len(), 21);
    assert_eq!(
        names(
            &tools_for(
                &host,
                Some(json!(["translate_presentation", "index_data", "x"]))
            )
            .await
        ),
        ["translate_presentation"]
    );
    // Names only the SDK's other catalogue knows select nothing.
    assert!(names(&tools_for(&host, Some(json!(["index_data"]))).await).is_empty());
}

#[test]
fn the_configuration_is_strict() {
    for bad in [
        json!({}),
        json!({"bucket_name": ""}),
        json!({"bucket_name": "Bad Bucket"}),
        json!({"bucket_name": "9start"}),
        json!({"bucket_name": "ok", "selected_tools": "fill_template"}),
        json!({"bucket_name": "ok", "selected_tools": [1]}),
        json!({"bucket_name": "ok", "selected_tools": [""]}),
    ] {
        let err = PptxToolkitConfig::parse(bad.as_object().expect("object"))
            .err()
            .unwrap_or_else(|| panic!("{bad} must be refused"));
        assert_eq!(err.code(), PptxConfigErrorCode::InvalidConfiguration);
    }
    let long = "a".repeat(64);
    let err = PptxToolkitConfig::parse(json!({"bucket_name": long}).as_object().expect("object"))
        .err()
        .expect("too long");
    assert_eq!(err.code(), PptxConfigErrorCode::ResourceExhausted);
    assert_eq!(
        PptxToolkitConfig::parse(&settings(None))
            .expect("config")
            .bucket(),
        "bucket"
    );
}

fn pptx_version() -> Value {
    json!({"tools": [{
        "id": 7,
        "type": "pptx",
        "toolkit_name": "deck",
        "settings": {"bucket_name": "bucket", "selected_tools": ["to_markdown", "fill_template"]}
    }]})
}

#[tokio::test]
async fn materialization_needs_the_office_capability_on_the_artifact_authority() {
    let host = Arc::new(FakeHost::default());
    let platform: Arc<dyn PlatformWriter> = Arc::new(NoPlatform);
    let version = pptx_version();
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());

    // Office lent: the toolkit materializes with its selected tools.
    let lent = ArtifactToolAuthority::new(Arc::clone(&platform))
        .with_office(Arc::clone(&host) as SharedOfficeToolHost);
    let (toolsets, refused) = materialize_configured_toolsets_with_artifact_authority(
        &snapshot,
        &policy(),
        &Map::new(),
        Some(&lent),
    )
    .await
    .expect("materialized");
    assert!(refused.is_empty() || toolsets.len() == 1);
    assert_eq!(toolsets.len(), 1);
    let ctx = context() as Arc<dyn ReadonlyContext>;
    let mut names = Vec::new();
    for tool in toolsets[0].tools(ctx).await.expect("tools") {
        names.push(tool.name().to_owned());
    }
    names.sort();
    assert_eq!(names, ["fill_template", "to_markdown"]);

    // Artifact authority without Office, and no authority at all: skipped as
    // unsupported, never bound to tools that would fail on every call.
    let bare = ArtifactToolAuthority::new(platform);
    let config = PptxToolkitConfig::parse(&settings(None)).expect("config");
    assert!(
        build_pptx_toolset("deck", &config, &policy(), &bare)
            .expect("builds")
            .is_none()
    );
    for authority in [Some(&bare), None] {
        let result = materialize_configured_toolsets_with_artifact_authority(
            &snapshot,
            &policy(),
            &Map::new(),
            authority,
        )
        .await;
        match result {
            Ok((toolsets, _)) => assert!(toolsets.is_empty(), "skipped, not bound"),
            Err(error) => panic!("an unserved toolkit is skipped, not fatal: {error:?}"),
        }
    }
}

// -- Authoring, end to end ------------------------------------------------

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn a_deck_is_authored_edited_read_previewed_and_exported() {
    let rig = Rig::new().await;
    let out = rig
        .text(
            "create_presentation",
            json!({"file_name": "q3.pptx", "title": "Q3 review", "subtitle": "Finance"}),
        )
        .await;
    assert!(
        out.starts_with("Created `q3.pptx` in bucket `bucket` ("),
        "{out}"
    );
    // It will not replace a file unless asked.
    let again = rig
        .text("create_presentation", json!({"file_name": "q3.pptx"}))
        .await;
    assert!(
        again.contains("already exists") && again.contains("overwrite"),
        "{again}"
    );
    assert!(
        rig.text(
            "create_presentation",
            json!({"file_name": "q3.pptx", "overwrite": true, "title": "Again"})
        )
        .await
        .starts_with("Created")
    );

    let add = rig
        .text(
            "add_slide",
            json!({"file_name": "q3.pptx", "title": "Results",
                   "bullets": ["Revenue up", {"text": "EMEA", "level": 1, "bold": true}],
                   "notes": "Open with the headline"}),
        )
        .await;
    assert!(add.starts_with("Added slide 2 (title_content)"), "{add}");
    rig.text(
        "add_slide",
        json!({"file_name": "q3.pptx", "layout": "blank", "position": 1}),
    )
    .await;
    let md = rig.markdown("q3.pptx");
    assert!(
        md.contains("Again") && md.contains("Results") && md.contains("Revenue up"),
        "{md}"
    );
    assert!(md.contains("Open with the headline"), "{md}");

    // Slide 1 is the new blank one now; author onto slide 3 ("Results") and 1.
    let table = rig
        .text(
            "add_table",
            json!({"file_name": "q3.pptx", "slide": 1,
                   "rows": [["Region", "Revenue"], ["EMEA", "12"], ["APAC", "9"]]}),
        )
        .await;
    assert!(table.contains("3 x 2 table"), "{table}");
    let cell = rig
        .text(
            "set_table_cell",
            json!({"file_name": "q3.pptx", "slide": 1, "row": 2, "column": 2, "text": "13 & rising"}),
        )
        .await;
    assert!(cell.starts_with("Set row 2, column 2"), "{cell}");
    let md = rig.markdown("q3.pptx");
    assert!(md.contains("13 & rising"), "{md}");
    let out_of_range = rig
        .text(
            "set_table_cell",
            json!({"file_name": "q3.pptx", "slide": 1, "row": 9, "column": 1, "text": "x"}),
        )
        .await;
    assert!(out_of_range.contains("outside"), "{out_of_range}");
    let no_table = rig
        .text(
            "set_table_cell",
            json!({"file_name": "q3.pptx", "slide": 3, "row": 1, "column": 1, "text": "x"}),
        )
        .await;
    assert!(no_table.contains("no table 1"), "{no_table}");

    let chart = rig
        .text(
            "add_chart",
            json!({"file_name": "q3.pptx", "slide": 1, "chart_type": "column",
                   "categories": ["Q1", "Q2"], "title": "Revenue", "data_labels": true,
                   "series": [{"name": "FY25", "values": [1.5, 2.5]}]}),
        )
        .await;
    assert!(chart.contains("render_preview does not"), "{chart}");
    let bad_chart = rig
        .text(
            "add_chart",
            json!({"file_name": "q3.pptx", "slide": 1, "categories": ["Q1", "Q2"],
                   "series": [{"name": "FY25", "values": [1.5]}]}),
        )
        .await;
    assert!(bad_chart.contains("one per category"), "{bad_chart}");

    rig.host.put("logo.png", TINY_PNG.to_vec());
    let image = rig
        .text(
            "add_image",
            json!({"file_name": "q3.pptx", "slide": 1, "image_file_name": "logo.png",
                   "x": 7.0, "y": 0.2, "width": 2.0, "height": 1.0}),
        )
        .await;
    assert!(image.starts_with("Added a picture to slide 1"), "{image}");
    let not_image = rig
        .text(
            "add_image",
            json!({"file_name": "q3.pptx", "slide": 1, "image_file_name": "q3.pptx"}),
        )
        .await;
    assert!(!not_image.starts_with("Added"), "{not_image}");

    rig.text(
        "set_title",
        json!({"file_name": "q3.pptx", "slide": 3, "title": "Results, final"}),
    )
    .await;
    rig.text(
        "set_notes",
        json!({"file_name": "q3.pptx", "slide": 3, "notes": "Pause here"}),
    )
    .await;
    rig.text(
        "add_bullets",
        json!({"file_name": "q3.pptx", "slide": 3, "items": ["One more"]}),
    )
    .await;
    rig.text(
        "set_text",
        json!({"file_name": "q3.pptx", "slide": 1, "text": "A footnote", "x": 0.5, "y": 6.5}),
    )
    .await;
    let theme = rig
        .text(
            "apply_theme",
            json!({"file_name": "q3.pptx", "accent": "C00000", "heading_font": "Georgia"}),
        )
        .await;
    // This deck holds a table, a chart and a picture, which replacing the
    // theme would drop: refused, and the file is as it was.
    let before_theme = rig.host.get("q3.pptx");
    assert!(
        theme.starts_with("Nothing was saved: this edit would have removed"),
        "{theme}"
    );
    assert!(
        theme.contains("1 table") && theme.contains("1 chart") && theme.contains("1 picture"),
        "{theme}"
    );
    assert_eq!(rig.host.get("q3.pptx"), before_theme);
    assert!(
        rig.text("apply_theme", json!({"file_name": "q3.pptx"}))
            .await
            .contains("at least one")
    );
    let md = rig.markdown("q3.pptx");
    for needle in [
        "Results, final",
        "Pause here",
        "One more",
        "A footnote",
        "Region",
    ] {
        assert!(md.contains(needle), "{needle}: {md}");
    }

    // set_text on an existing shape, addressed by number and by name.
    let listing = rig
        .call(
            "list_placeholders",
            json!({"file_name": "q3.pptx", "include_all": true}),
        )
        .await;
    let frames = listing["placeholders"].as_array().expect("rows");
    let title = frames
        .iter()
        .find(|r| {
            r["text"]
                .as_str()
                .is_some_and(|t| t.contains("Results, final"))
        })
        .unwrap_or_else(|| panic!("the title frame: {listing}"));
    let shape = title["shape"].as_u64().expect("shape number");
    let replaced = rig
        .text(
            "set_text",
            json!({"file_name": "q3.pptx", "slide": 3, "shape": shape, "text": "Results (v2)"}),
        )
        .await;
    assert!(
        replaced.starts_with("Replaced the text of shape"),
        "{replaced}"
    );
    assert!(rig.markdown("q3.pptx").contains("Results (v2)"));
    let by_name = rig
        .text(
            "set_text",
            json!({"file_name": "q3.pptx", "slide": 3, "shape": title["name"], "text": "Results (v3)"}),
        )
        .await;
    assert!(by_name.starts_with("Replaced"), "{by_name}");
    let missing = rig
        .text(
            "set_text",
            json!({"file_name": "q3.pptx", "slide": 3, "shape": "No such", "text": "x"}),
        )
        .await;
    assert!(missing.contains("has no text shape"), "{missing}");

    // Slide moves and deletes keep the numbering honest.
    let moved = rig
        .text(
            "move_slide",
            json!({"file_name": "q3.pptx", "slide": 3, "to_slide": 1}),
        )
        .await;
    assert!(moved.starts_with("Moved slide 3 to position 1"), "{moved}");
    assert!(
        rig.text("delete_slide", json!({"file_name": "q3.pptx", "slide": 9}))
            .await
            .contains("deck has 3 slides")
    );
    let deleted = rig
        .text("delete_slide", json!({"file_name": "q3.pptx", "slide": 3}))
        .await;
    assert!(deleted.starts_with("Deleted slide 3"), "{deleted}");

    // Preview: a PNG artifact and the chart data beside it, with the caveat.
    let preview = rig
        .call(
            "render_preview",
            json!({"file_name": "q3.pptx", "slide": 2}),
        )
        .await;
    assert_eq!(preview["status"], "success", "{preview}");
    assert_eq!(preview["image"], "q3-slide-2.png");
    assert_eq!(preview["bucket"], "bucket");
    assert!(rig.host.get("q3-slide-2.png").starts_with(b"\x89PNG"));
    // Slide 2 carries the table, the chart and the picture.
    let with_chart = &preview;
    let chart = &with_chart["charts"][0];
    assert!(
        with_chart["note"]
            .as_str()
            .expect("note")
            .contains("charts are drawn as grey placeholders"),
        "{with_chart}"
    );
    // A slide without a chart says nothing about charts.
    let plain = rig
        .call(
            "render_preview",
            json!({"file_name": "q3.pptx", "slide": 1, "output_file_name": "p1.png"}),
        )
        .await;
    assert_eq!(plain["charts"], json!([]), "{plain}");
    assert!(
        plain["note"]
            .as_str()
            .expect("note")
            .starts_with("Approximate preview: SVG"),
        "{plain}"
    );
    assert_eq!(chart["type"], "column");
    assert_eq!(chart["title"], "Revenue");
    assert_eq!(chart["categories"], json!(["Q1", "Q2"]));
    assert_eq!(chart["series"][0]["name"], "FY25");
    assert_eq!(chart["series"][0]["values"], json!([1.5, 2.5]));

    let pdf = rig
        .text("export_pdf", json!({"file_name": "q3.pptx"}))
        .await;
    assert!(
        pdf.starts_with("Exported `q3.pdf` in bucket `bucket`"),
        "{pdf}"
    );
    assert!(rig.host.get("q3.pdf").starts_with(b"%PDF"));

    // Output to another name leaves the source alone.
    let before = rig.host.get("q3.pptx");
    rig.text(
        "set_notes",
        json!({"file_name": "q3.pptx", "slide": 1, "notes": "Copy", "output_file_name": "copy.pptx"}),
    )
    .await;
    assert_eq!(rig.host.get("q3.pptx"), before);
    assert!(rig.markdown("copy.pptx").contains("Copy"));
}

#[tokio::test]
async fn apply_theme_works_on_a_plain_deck_and_set_notes_refuses_to_lose_content() {
    let rig = Rig::new().await;
    rig.text(
        "create_presentation",
        json!({"file_name": "p.pptx", "title": "Plain"}),
    )
    .await;
    let done = rig
        .text("apply_theme", json!({"file_name": "p.pptx", "accent": "C00000", "heading_font": "Georgia", "colors": {"dk2": "1F3864"}}))
        .await;
    assert!(
        done.starts_with("Applied the theme. Saved `p.pptx` in bucket `bucket`"),
        "{done}"
    );
    assert!(rig.markdown("p.pptx").contains("Plain"));
    // A table, then notes on a slide that has none: the library would rewrite
    // the file and drop the table, so the edit is refused and nothing changes.
    rig.text(
        "add_table",
        json!({"file_name": "p.pptx", "slide": 1, "rows": [["a", "b"]]}),
    )
    .await;
    let before = rig.host.get("p.pptx");
    let refused = rig
        .text(
            "set_notes",
            json!({"file_name": "p.pptx", "slide": 1, "notes": "hello"}),
        )
        .await;
    assert!(
        refused.starts_with("Nothing was saved") && refused.contains("1 table"),
        "{refused}"
    );
    assert_eq!(rig.host.get("p.pptx"), before);
}

#[tokio::test]
async fn design_check_reports_what_it_can_measure() {
    let rig = Rig::new().await;
    let made = rig
        .text("create_presentation", json!({"file_name": "d.pptx"}))
        .await;
    assert!(made.starts_with("Created"), "{made}");
    for layout in ["title_content", "blank"] {
        let added = rig
            .text(
                "add_slide",
                json!({"file_name": "d.pptx", "layout": layout}),
            )
            .await;
        assert!(added.starts_with("Added slide"), "{added}");
    }
    // A title placeholder with nothing in it.
    rig.text(
        "set_title",
        json!({"file_name": "d.pptx", "slide": 2, "title": " "}),
    )
    .await;
    rig.text(
        "set_text",
        json!({"file_name": "d.pptx", "slide": 3, "x": 0.5, "y": 0.5, "width": 1.0, "height": 0.3,
               "text": "This sentence is much too long to fit a one inch by a third of an inch box at eighteen points."}),
    )
    .await;
    let report = rig
        .call("design_check", json!({"file_name": "d.pptx"}))
        .await;
    assert_eq!(report["slides_checked"], 3, "{report}");
    let codes: Vec<(u64, &str)> = report["issues"]
        .as_array()
        .expect("issues")
        .iter()
        .map(|i| {
            (
                i["slide"].as_u64().expect("slide"),
                i["code"].as_str().expect("code"),
            )
        })
        .collect();
    for slide in 1..=3 {
        assert!(codes.contains(&(slide, "missing_title")), "{codes:?}");
    }
    assert!(codes.contains(&(3, "text_overflow")), "{codes:?}");
    assert!(report["warnings"].as_u64().expect("warnings") >= 1);
    assert!(
        report["note"]
            .as_str()
            .expect("note")
            .contains("nothing is rendered")
    );
    // One slide only.
    let one = rig
        .call("design_check", json!({"file_name": "d.pptx", "slide": 3}))
        .await;
    assert_eq!(one["slides_checked"], 1);
    assert!(
        one["issues"]
            .as_array()
            .expect("issues")
            .iter()
            .all(|i| i["slide"] == 3),
        "{one}"
    );
    assert!(
        rig.text("design_check", json!({"file_name": "d.pptx", "slide": 7}))
            .await
            .contains("does not exist")
    );
}

#[test]
fn design_rules_over_explicit_geometry_fonts_and_colours() {
    use elitea_office_core::{ShapeFacts, ShapeKind, SlideFacts};
    let shape = |name: &str, frame: Option<Rect>, text: &str| ShapeFacts {
        index: 0,
        text_index: Some(0),
        name: name.to_owned(),
        kind: ShapeKind::Text,
        placeholder: None,
        frame,
        text: text.to_owned(),
        font_sizes_pt: Vec::new(),
        text_color: None,
        fill_color: None,
    };
    let mut small = shape("small", Some(Rect::inches(1.0, 1.0, 4.0, 1.0)), "tiny");
    small.font_sizes_pt = vec![9.0];
    let mut dull = shape(
        "dull",
        Some(Rect::inches(1.0, 2.5, 4.0, 1.0)),
        "grey on grey",
    );
    dull.text_color = Some("777777".into());
    dull.fill_color = Some("888888".into());
    let mut fine = shape(
        "fine",
        Some(Rect::inches(1.0, 4.0, 4.0, 1.0)),
        "black on white",
    );
    fine.text_color = Some("000000".into());
    fine.fill_color = Some("FFFFFF".into());
    let wide = shape(
        "wide",
        Some(Rect::inches(8.0, 5.0, 4.0, 1.0)),
        "off the edge",
    );
    let mut empty = shape("empty", None, "  ");
    empty.placeholder = Some("body".into());
    let mut footer = shape("footer", None, "");
    footer.placeholder = Some("ftr".into());
    let a = shape("a", Some(Rect::inches(1.0, 6.0, 3.0, 1.0)), "one");
    let b = shape("b", Some(Rect::inches(1.5, 6.1, 3.0, 1.0)), "two");
    let facts = vec![SlideFacts {
        slide: 0,
        size: (9_144_000, 6_858_000),
        shapes: vec![small, dull, fine, wide, a, b, empty, footer],
        tables: Vec::new(),
        charts: Vec::new(),
        notes: String::new(),
    }];
    let report = ops::design_from_facts(&facts, None);
    let codes: Vec<(&str, &str)> = report["issues"]
        .as_array()
        .expect("issues")
        .iter()
        .map(|i| {
            (
                i["shape"].as_str().unwrap_or(""),
                i["code"].as_str().expect("code"),
            )
        })
        .collect();
    assert!(codes.contains(&("empty", "empty_placeholder")), "{codes:?}");
    assert!(
        !codes.iter().any(|(s, _)| *s == "footer"),
        "a footer may be empty: {codes:?}"
    );
    assert!(codes.contains(&("small", "small_font")), "{codes:?}");
    assert!(codes.contains(&("dull", "low_contrast")), "{codes:?}");
    assert!(codes.contains(&("wide", "off_slide")), "{codes:?}");
    assert!(codes.contains(&("a", "overlap")), "{codes:?}");
    assert!(
        !codes.iter().any(|(s, c)| *s == "fine" && *c != "overlap"),
        "{codes:?}"
    );
    assert!(codes.contains(&("", "missing_title")));
}

// -- The deterministic halves ---------------------------------------------

#[tokio::test]
async fn placeholders_are_listed_and_replaced_without_a_model() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    let listing = rig
        .call("list_placeholders", json!({"file_name": "t.pptx"}))
        .await;
    assert_eq!(listing["total_locations"], 3, "{listing}");
    assert_eq!(listing["unique_texts"], 3);
    let rows = listing["placeholders"].as_array().expect("rows");
    assert!(
        rows.iter()
            .any(|r| r["text"] == "{{title}}" && r["location"] == "text_frame" && r["slide"] == 1)
    );
    assert!(rows.iter().any(|r| r["text"] == "[PLACEHOLDER]"
        && r["location"] == "table_cell"
        && r["row"] == 2
        && r["column"] == 2));
    assert!(listing["note"].as_str().expect("note").contains("SmartArt"));
    let all = rig
        .call(
            "list_placeholders",
            json!({"file_name": "t.pptx", "include_all": true}),
        )
        .await;
    assert!(all["total_locations"].as_u64().expect("n") > 3);

    // A whole-frame key, a substring key and a table cell; one key matches nothing.
    let result = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "t.pptx", "output_file_name": "filled.pptx",
                   "replacements": {"{{title}}": "Annual report", "{{name}}": "Ada",
                                    "[PLACEHOLDER]": "42", "{{nothing}}": "x"}}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(result["replacements_applied"], 3, "{result}");
    assert_eq!(result["keys_not_found"], json!(["{{nothing}}"]));
    let md = rig.markdown("filled.pptx");
    assert!(
        md.contains("Annual report") && md.contains("Hello Ada, welcome") && md.contains("42"),
        "{md}"
    );
    assert!(!md.contains("{{") && !md.contains("[PLACEHOLDER]"), "{md}");
    assert!(
        rig.markdown("t.pptx").contains("{{title}}"),
        "the source is untouched"
    );
    // Nothing left to list.
    let none = rig
        .call("list_placeholders", json!({"file_name": "filled.pptx"}))
        .await;
    assert_eq!(none["total_locations"], 0);
    assert!(
        rig.text(
            "replace_placeholders",
            json!({"file_name": "t.pptx", "replacements": {}})
        )
        .await
        .contains("at least one")
    );
}

// -- fill_template ----------------------------------------------------------

#[tokio::test]
async fn fill_template_batches_calls_the_model_and_writes_the_sdk_result() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    rig.host.reply(
        "```json\n[{\"key\": \"{{title}}\", \"content\": \"Annual report\"},\n {\"key\": \"Hello {{name}}, welcome\", \"content\": \"Hello Ada\"},\n {\"key\": \"[PLACEHOLDER]\", \"content\": \"42\"}]\n```",
    );
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "out.pptx",
                   "content_description": "Annual report for Ada"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(result["url"], "out.pptx");
    assert_eq!(result["bucket"], "bucket");
    assert_eq!(
        result["message"],
        "Successfully filled 3 unique placeholders (3 total locations) across 2 slides. Saved as out.pptx"
    );
    let stats = &result["stats"];
    assert_eq!(stats["unique_placeholders"], 3);
    assert_eq!(stats["total_placeholder_locations"], 3);
    assert_eq!(stats["text_frame_updates"], 2);
    assert_eq!(stats["table_updates"], 1);
    assert_eq!(stats["smartart_updates"], 0);
    assert_eq!(stats["total_slides"], 2);
    assert_eq!(stats["batches_processed"], 1);
    let md = rig.markdown("out.pptx");
    assert!(
        md.contains("Annual report") && md.contains("Hello Ada") && md.contains("42"),
        "{md}"
    );
    assert!(!md.contains("{{"), "{md}");
    // One call, with the SDK's prompt and no images.
    let calls = rig.host.model_calls();
    assert_eq!(calls.len(), 1);
    assert!(
        calls[0]
            .prompt
            .starts_with("I need content for 3 placeholders in a PowerPoint presentation.")
    );
    assert!(calls[0].prompt.contains("Annual report for Ada"));
    assert!(calls[0].images_png.is_empty());
}

#[tokio::test]
async fn fill_template_batch_size_bounds_the_model_calls() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    for key in ["{{title}}", "Hello {{name}}, welcome", "[PLACEHOLDER]"] {
        rig.host
            .reply(&json!([{"key": key, "content": "filled"}]).to_string());
    }
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "out.pptx",
                   "content_description": "x", "batch_size": 1}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(result["stats"]["batches_processed"], 3);
    let calls = rig.host.model_calls();
    assert_eq!(calls.len(), 3, "one model call per batch of one");
    for call in &calls {
        assert!(call.prompt.contains("I need content for 1 placeholders"));
    }
}

#[tokio::test]
async fn fill_template_never_leaves_a_marker_silently_and_names_failures() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    // The model answers only one of three: the others get the SDK's marker.
    rig.host
        .reply(r#"[{"key": "{{title}}", "content": "Only this"}]"#);
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "o.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    let md = rig.markdown("o.pptx");
    assert!(
        md.contains("Only this") && md.contains("[Content for: [PLACEHOLDER]]"),
        "{md}"
    );

    // No placeholders: the SDK's warning, nothing written.
    rig.host.put("plain.pptx", {
        let mut d = Deck::new(&test_limits());
        let s = d.add_slide(SlideLayout::Blank).unwrap();
        d.add_text(s, "nothing here", Rect::inches(1.0, 1.0, 3.0, 1.0))
            .unwrap();
        d.save().unwrap()
    });
    let none = rig
        .call(
            "fill_template",
            json!({"file_name": "plain.pptx", "output_file_name": "p2.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(none["status"], "warning");
    assert_eq!(
        none["message"],
        "No placeholders found in the presentation. Nothing to fill."
    );
    assert!(none["url"].is_null());
    assert!(!rig.host.has("p2.pptx"));

    // A model failure, a missing file and a corrupt file are error results.
    rig.host.fail_model(OfficeFailure::BudgetRefused);
    let budget = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "o2.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(budget["status"], "error");
    assert!(
        budget["message"]
            .as_str()
            .expect("message")
            .starts_with("Failed to fill template: Could not call the model")
    );
    assert!(
        !rig.host.has("o2.pptx"),
        "nothing is saved when the model fails"
    );
    let missing = rig
        .call(
            "fill_template",
            json!({"file_name": "nope.pptx", "output_file_name": "o3.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(missing["status"], "error");
    assert!(
        missing["message"]
            .as_str()
            .expect("message")
            .contains("no such file")
    );
    rig.host.put("junk.pptx", b"not a zip at all".to_vec());
    let junk = rig
        .call(
            "fill_template",
            json!({"file_name": "junk.pptx", "output_file_name": "o4.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(junk["status"], "error");
    assert!(
        junk["message"]
            .as_str()
            .expect("message")
            .contains("not a valid PowerPoint")
    );
}

#[tokio::test]
async fn a_named_pdf_sends_the_templates_own_slides_as_visual_context() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    rig.host
        .reply(r#"[{"key": "{{title}}", "content": "A"}, {"key": "Hello {{name}}, welcome", "content": "B"}, {"key": "[PLACEHOLDER]", "content": "C"}]"#);
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "o.pptx", "content_description": "x",
                   "pdf_file_name": "t.pdf"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(result["stats"]["visual_context_slides"], 2);
    let calls = rig.host.model_calls();
    assert_eq!(calls[0].images_png.len(), 2);
    assert!(
        calls[0]
            .images_png
            .iter()
            .all(|i| i.starts_with(b"\x89PNG"))
    );
}

// -- translate_presentation -------------------------------------------------

#[tokio::test]
async fn translate_presentation_translates_every_paragraph_and_cell() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", template());
    // Unique paragraphs in order: title, greeting, static text, Region, placeholder cell.
    rig.host.reply(
        r#"[{"index": 1, "translation": "{{titulo}}"},
            {"index": 2, "translation": "Hola {{name}}, bienvenido"},
            {"index": 3, "translation": "Texto estatico"},
            {"index": 4, "translation": "Region (es)"},
            {"index": 5, "translation": "[MARCADOR]"}]"#,
    );
    let result = rig
        .call(
            "translate_presentation",
            json!({"file_name": "t.pptx", "output_file_name": "es.pptx", "target_language": "es"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(
        result["message"],
        "Successfully translated presentation to Spanish and saved as es.pptx"
    );
    assert_eq!(result["url"], "es.pptx");
    let md = rig.markdown("es.pptx");
    for needle in [
        "Hola {{name}}, bienvenido",
        "Texto estatico",
        "Region (es)",
        "[MARCADOR]",
    ] {
        assert!(md.contains(needle), "{needle}: {md}");
    }
    assert!(!md.contains("Static text"), "{md}");
    let calls = rig.host.model_calls();
    assert_eq!(calls.len(), 1);
    assert!(
        calls[0]
            .prompt
            .starts_with("Please translate the following 5 texts to Spanish.")
    );
    assert!(calls[0].prompt.contains("\"index\": 5"));
    // The source is untouched.
    assert!(rig.markdown("t.pptx").contains("Static text"));
}

#[tokio::test]
async fn translation_continues_indexes_across_batches_and_keeps_what_it_cannot_translate() {
    let rig = Rig::new().await;
    // 25 distinct paragraphs on one slide: two batches (20 + 5).
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    for i in 0..25 {
        d.add_text(
            s,
            &format!("line {i}"),
            Rect::inches(0.5, 0.2 + f64::from(i) * 0.25, 4.0, 0.2),
        )
        .unwrap();
    }
    rig.host.put("many.pptx", d.save().unwrap());
    let first: Vec<Value> = (1..=20)
        .map(|i| json!({"index": i, "translation": format!("ligne {}", i - 1)}))
        .collect();
    rig.host.reply(&Value::Array(first).to_string());
    // The second reply covers only two of five; the three left out are asked
    // for once more, and that reply gives one of them.
    rig.host.reply(
        r#"[{"index": 21, "translation": "ligne 20"}, {"index": 25, "translation": "ligne 24"}]"#,
    );
    rig.host
        .reply(r#"[{"index": 26, "translation": "ligne 21"}]"#);
    let result = rig
        .call(
            "translate_presentation",
            json!({"file_name": "many.pptx", "output_file_name": "fr.pptx", "target_language": "French"}),
        )
        .await;
    // Two paragraphs are still missing: the result says so, never "Successfully".
    assert_eq!(result["status"], "partial", "{result}");
    assert_eq!(result["untranslated_paragraphs"], 2);
    assert_eq!(result["translated_paragraphs"], 23);
    let message = result["message"].as_str().expect("message");
    assert!(
        message.starts_with("Partially translated presentation to French"),
        "{message}"
    );
    assert!(message.contains("2 of 25 paragraphs"), "{message}");
    assert!(!message.contains("Successfully"), "{message}");
    let calls = rig.host.model_calls();
    assert_eq!(
        calls.len(),
        3,
        "two batches and one retry of what was left out"
    );
    assert!(
        calls[2]
            .prompt
            .contains("translate the following 3 texts to French")
    );
    assert!(calls[2].prompt.contains("\"index\": 26"));
    assert!(
        calls[0]
            .prompt
            .contains("translate the following 20 texts to French")
    );
    assert!(
        calls[1]
            .prompt
            .contains("translate the following 5 texts to French")
    );
    assert!(calls[1].prompt.contains("\"index\": 21"));
    let md = rig.markdown("fr.pptx");
    assert!(
        md.contains("ligne 0") && md.contains("ligne 20") && md.contains("ligne 24"),
        "{md}"
    );
    assert!(md.contains("ligne 21"), "the retry's text landed: {md}");
    assert!(
        md.contains("line 22") && md.contains("line 23"),
        "an uncovered text keeps its original: {md}"
    );
}

#[tokio::test]
async fn an_unreachable_platform_and_a_missing_file_are_sentences() {
    let host = Arc::new(FakeHost {
        unreachable: true,
        ..FakeHost::default()
    });
    let tools = tools_for(&host, None).await;
    let tool = tools
        .iter()
        .find(|t| t.name() == "to_markdown")
        .expect("tool");
    let out = tool
        .execute(
            context() as Arc<dyn ToolContext>,
            json!({"file_name": "a.pptx"}),
        )
        .await
        .expect("result");
    assert!(
        out.as_str()
            .expect("sentence")
            .contains("could not be reached"),
        "{out}"
    );

    let rig = Rig::new().await;
    let out = rig
        .text("to_markdown", json!({"file_name": "a.pptx"}))
        .await;
    assert!(out.contains("no such file"), "{out}");
    let out = rig
        .text("to_markdown", json!({"file_name": "../a.pptx"}))
        .await;
    assert!(out.contains(".."), "{out}");
    let out = rig.text("to_markdown", json!({})).await;
    assert!(out.contains("`file_name` is required"), "{out}");
    rig.host.put("t.pptx", template());
    let out = rig
        .text("delete_slide", json!({"file_name": "t.pptx", "slide": 0}))
        .await;
    assert!(out.contains("does not exist"), "{out}");
    let out = rig
        .text(
            "add_slide",
            json!({"file_name": "t.pptx", "layout": "weird"}),
        )
        .await;
    assert!(out.contains("Unknown layout"), "{out}");
}

#[tokio::test]
async fn hostile_bytes_are_refused_with_a_sentence_not_a_panic() {
    let rig = Rig::new().await;
    // A zip that is not a deck, a truncated deck, and garbage.
    let good = template();
    rig.host.put("trunc.pptx", good[..good.len() / 2].to_vec());
    rig.host.put("junk.pptx", vec![0u8; 1024]);
    for name in ["trunc.pptx", "junk.pptx"] {
        for tool in [
            "to_markdown",
            "design_check",
            "list_placeholders",
            "export_pdf",
        ] {
            let out = rig.call(tool, json!({"file_name": name})).await;
            let text = out.as_str().expect("a sentence").to_owned();
            assert!(
                text.contains("not a valid PowerPoint"),
                "{tool} {name}: {text}"
            );
        }
    }
}

#[tokio::test]
async fn a_long_deck_is_cut_with_a_notice() {
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    for i in 0..300 {
        let s = d.add_slide(SlideLayout::Blank).unwrap();
        d.add_text(s, &"w".repeat(2_000), Rect::inches(0.5, 0.5, 8.0, 2.0))
            .unwrap();
        let _ = i;
    }
    rig.host.put("big.pptx", d.save().unwrap());
    let md = rig
        .text("to_markdown", json!({"file_name": "big.pptx"}))
        .await;
    assert!(
        md.contains("[Truncated at 200000 characters"),
        "{}",
        &md[md.len() - 120..]
    );
}

// -- Prompts and replies ------------------------------------------------------

#[test]
fn the_fill_prompt_is_the_sdks() {
    let prompt = model::fill_prompt(&["{{a}}".to_owned()], "desc");
    assert_eq!(
        prompt,
        "I need content for 1 placeholders in a PowerPoint presentation.\n\n\
<Data Available for use>\ndesc\n</Data Available for use>\n\n\
<Placeholders to fill>\n[\n  {\n    \"placeholder\": \"{{a}}\",\n    \"key\": \"{{a}}\"\n  }\n]\n</Placeholders to fill>\n\n\
Please provide content for ALL placeholders listed above.\n\
Return your response as a JSON array matching each placeholder by its key:\n\
[{\"key\": \"{{placeholder_name}}\", \"content\": \"generated content\"}, ...]\n\n\
IMPORTANT:\n\
- Use the exact \"key\" value from the input to identify each placeholder\n\
- Preserve any newlines (\\n) if needed in the content\n\
- Match the tone and style suggested by each placeholder\n\
- Base your content on the provided data description\n\
- Keep content concise and appropriate for PowerPoint slides"
    );
}

#[test]
fn the_translate_prompt_is_the_sdks() {
    let prompt =
        model::translate_prompt(&["Hi".to_owned(), "Bye \"now\"".to_owned()], "German", 20);
    assert!(prompt.starts_with(
        "Please translate the following 2 texts to German.\nThe texts are provided in JSON format below. Some texts may contain newlines or special characters.\n\nReturn your response as a JSON array with the same structure, keeping the same index numbers.\nFormat: [{\"index\": 21, \"translation\": \"translated text\"}, {\"index\": 22, \"translation\": \"translated text\"}, ...]\n\nIMPORTANT: Preserve any newlines (\\n) in the translated text. Only translate the content, keep the formatting.\n\nTexts to translate:\n[\n  {\n    \"index\": 21,\n    \"text\": \"Hi\"\n  }"
    ));
    assert!(prompt.contains("\"text\": \"Bye \\\"now\\\"\""));
    assert_eq!(model::language_name("ua"), "Ukrainian");
    assert_eq!(model::language_name("ES"), "Spanish");
    assert_eq!(model::language_name("Klingon"), "Klingon");
}

#[test]
fn fill_replies_parse_as_the_sdk_does() {
    let keys: Vec<String> = ["{{a}}", "{{b}}", "{{c}}"].map(str::to_owned).to_vec();
    // Fenced, with a prefix; bare; with the SDK's `placeholder`/`text` aliases.
    let fenced = model::parse_fill_reply(
        "Sure!\n```json\n[{\"key\": \"{{a}}\", \"content\": \"A\"}]\n```\nDone",
        &keys,
    );
    assert_eq!(fenced["{{a}}"], "A");
    assert_eq!(fenced["{{b}}"], "[Content for: {{b}}]");
    let bare = model::parse_fill_reply(
        r#"[{"placeholder": "{{b}}", "text": "B"}, {"key": "", "content": "dropped"}]"#,
        &keys,
    );
    assert_eq!(bare["{{b}}"], "B");
    assert_eq!(bare.len(), 3);
    // Not JSON: the SDK's line fallback, by order, quotes stripped.
    let lines = model::parse_fill_reply("\"first\"\n'second'\n{ignored}\nthird", &keys);
    assert_eq!(lines["{{a}}"], "first");
    assert_eq!(lines["{{b}}"], "second");
    assert_eq!(lines["{{c}}"], "third");
    let content_lines =
        model::parse_fill_reply("{\"content\": \"x\"}\n{\"content\": \"y\"}", &keys);
    assert_eq!(content_lines["{{a}}"], "x");
    // Non-string content is kept as its JSON text rather than lost.
    let number = model::parse_fill_reply(r#"[{"key": "{{a}}", "content": 5}]"#, &keys);
    assert_eq!(number["{{a}}"], "5");
}

#[test]
fn translation_replies_parse_by_index_then_position_then_lines() {
    let texts: Vec<String> = ["one", "two", "three"].map(str::to_owned).to_vec();
    let some = |v: &[Option<&str>]| -> Vec<Option<String>> {
        v.iter().map(|t| t.map(str::to_owned)).collect()
    };
    let by_index = model::parse_translation_reply(
        r#"```json
[{"index": 3, "translation": "tres"}, {"index": 1, "translation": "uno"}]
```"#,
        &texts,
        0,
    );
    // The one the reply does not give is None: the caller asks again.
    assert_eq!(by_index, some(&[Some("uno"), None, Some("tres")]));
    // Indexes continue across batches.
    let later = model::parse_translation_reply(
        r#"[{"index": 11, "translation": "uno"}, {"index": 12, "translation": "dos"}, {"index": 13, "translation": "tres"}]"#,
        &texts,
        10,
    );
    assert_eq!(later, some(&[Some("uno"), Some("dos"), Some("tres")]));
    // A mixed reply: the indexed items are used, the unindexed one is not guessed at.
    let mixed = model::parse_translation_reply(
        r#"[{"index": 2, "translation": "dos"}, {"translation": "stray"}]"#,
        &texts,
        0,
    );
    assert_eq!(mixed, some(&[None, Some("dos"), None]));
    // No indexes: position, but only when the reply has one entry per text.
    let positional = model::parse_translation_reply(
        r#"[{"translation": "uno"}, {"text": "dos"}, {"translation": "tres"}]"#,
        &texts,
        0,
    );
    assert_eq!(positional, some(&[Some("uno"), Some("dos"), Some("tres")]));
    let short =
        model::parse_translation_reply(r#"[{"translation": "uno"}, {"text": "dos"}]"#, &texts, 0);
    assert_eq!(short, some(&[None, None, None]));
    let lines = model::parse_translation_reply("1. uno\n2) \"dos\"\n3. tres", &texts, 0);
    assert_eq!(lines, some(&[Some("uno"), Some("dos"), Some("tres")]));
    let too_many = model::parse_translation_reply("1. uno\n2) dos\n3. tres\n4. extra", &texts, 0);
    assert_eq!(too_many, some(&[None, None, None]));
    let json_lines = model::parse_translation_reply(
        "{\"index\": 1, \"translation\": \"uno\"}\n{\"index\": 2, \"translation\": \"dos\"}",
        &texts,
        0,
    );
    assert_eq!(
        json_lines,
        some(&[None, None, None]),
        "bare JSON lines are not an array"
    );
    assert_eq!(
        model::parse_translation_reply("", &texts, 0),
        some(&[None, None, None])
    );
    // A blank translation of a real text is a missing one.
    let blank = model::parse_translation_reply(r#"[{"index": 1, "translation": "  "}]"#, &texts, 0);
    assert_eq!(blank, some(&[None, None, None]));
}

#[tokio::test]
async fn bullets_and_titles_reach_the_file_on_slides_that_have_no_placeholder_for_them() {
    let rig = Rig::new().await;
    rig.text("create_presentation", json!({"file_name": "b.pptx"}))
        .await;
    // A blank slide has no body placeholder: upstream reported the bullets
    // added and wrote nothing. They are put in a text box.
    let blank = rig
        .text("add_bullets", json!({"file_name": "b.pptx", "slide": 1, "items": ["Anything", {"text": "Deeper", "level": 1}]}))
        .await;
    assert!(blank.starts_with("Added 2 bullets to slide 1"), "{blank}");
    let md = rig.markdown("b.pptx");
    assert!(md.contains("Anything") && md.contains("Deeper"), "{md}");
    // A slide added to an opened deck starts blank too; its title and bullets
    // land in the file, with no notes call forcing a rewrite to save them.
    let added = rig
        .text("add_slide", json!({"file_name": "b.pptx", "layout": "title_content", "title": "Agenda", "bullets": ["One", "Two"]}))
        .await;
    assert!(added.starts_with("Added slide 2"), "{added}");
    let md = rig.markdown("b.pptx");
    for needle in ["Agenda", "One", "Two"] {
        assert!(md.contains(needle), "{needle}: {md}");
    }
}

#[test]
fn a_content_edit_is_judged_on_its_target_slide() {
    let l = test_limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::Blank).unwrap();
    let s1 = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s1, "Present on two", Rect::inches(1.0, 1.0, 3.0, 1.0))
        .unwrap();
    d.add_text(s0, "Anchor", Rect::inches(1.0, 1.0, 3.0, 1.0))
        .unwrap();
    let facts = elitea_office_core::facts_of_package(&d.save().unwrap(), &l).unwrap();
    let expect = |tool, args: Value, slides| ops::expected_gain(tool, &args, slides);
    // The words are on slide 2, and an add_bullets that did nothing on slide 1
    // (before == after) is refused: the same words elsewhere prove nothing.
    let no_op = ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::AddBullets,
            json!({"slide": 1, "items": ["Present on two"]}),
            2,
        ),
    )
    .unwrap_err();
    assert!(
        no_op.starts_with("The text did not appear on the slide"),
        "{no_op}"
    );
    // On the slide that has them it passes, whatever punctuation does.
    ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::AddBullets,
            json!({"slide": 2, "items": ["Present on two!"]}),
            2,
        ),
    )
    .unwrap();
    // A text box added with text already on the slide must be NEW there: the
    // count has to go up.
    let unchanged = ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::SetText,
            json!({"slide": 2, "text": "Present on two"}),
            2,
        ),
    )
    .unwrap_err();
    assert!(unchanged.contains("did not appear"), "{unchanged}");
    // Replacing the text of a named shape is judged on that shape.
    let named = ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::SetText,
            json!({"slide": 1, "shape": 1, "text": "Present on two"}),
            2,
        ),
    )
    .unwrap_err();
    assert!(named.contains("did not appear"), "{named}");
    ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::SetText,
            json!({"slide": 1, "shape": 1, "text": "Anchor"}),
            2,
        ),
    )
    .unwrap();
    // A table that did not arrive on its slide.
    let table = ops::check_expect(
        &facts,
        &facts,
        &expect(PptxTool::AddTable, json!({"slide": 2}), 2),
    )
    .unwrap_err();
    assert!(table.contains("did not save what was added"), "{table}");
    // A slide added with a title is judged on the new slide alone (it has no
    // "before"): slide 1 has no such title, so the add did not land.
    let added = ops::check_expect(
        &facts,
        &facts,
        &expect(
            PptxTool::AddSlide,
            json!({"title": "Present on two", "position": 1}),
            2,
        ),
    );
    assert!(added.is_err(), "{added:?}");
}

// -- Review round 1 ----------------------------------------------------------

fn runs_of(bytes: &[u8], slide: usize, shape: usize, paragraph: usize) -> Vec<(bool, String)> {
    let deck = Deck::open(bytes, &test_limits()).expect("deck");
    deck.shapes(slide).expect("shapes")[shape].paragraphs[paragraph]
        .runs
        .iter()
        .map(|r| (r.line_break, r.text.clone()))
        .collect()
}

fn one_box(text: &str) -> Vec<u8> {
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, text, Rect::inches(1.0, 1.0, 6.0, 1.0))
        .unwrap();
    Deck::open(&d.save().unwrap(), &test_limits())
        .unwrap()
        .save()
        .unwrap()
}

/// Finding 1: a frame is written whole or not at all. A frame with no
/// paragraph is refused before a byte is written; a frame of several
/// paragraphs takes every line.
#[test]
fn a_frame_is_written_whole_or_not_at_all() {
    let l = test_limits();
    let bytes = one_box("alpha");
    let mut o = ops::open_with_facts(&bytes, &l).unwrap();
    let mut shape = o.deck.shapes(0).unwrap()[0].clone();
    let location = |shape: &elitea_office_core::ShapeInfo| ops::Location {
        slide: 0,
        text: shape.text.clone(),
        name: shape.name.clone(),
        place: ops::Place::Frame(shape.clone()),
    };
    // No paragraph to write into: Ok(false), and the deck is untouched.
    let before = o.deck.save().unwrap();
    shape.paragraphs.clear();
    assert!(!ops::write_location(&mut o.deck, &location(&shape), "new").unwrap());
    assert_eq!(o.deck.save().unwrap(), before);
    // A frame with two paragraphs gets all of three lines: two paragraphs, the
    // surplus line kept in the last one as a break.
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s, "t").unwrap();
    d.add_bullets(s, &[Bullet::new("one"), Bullet::new("two")])
        .unwrap();
    let two = Deck::open(&d.save().unwrap(), &l).unwrap().save().unwrap();
    let mut o = ops::open_with_facts(&two, &l).unwrap();
    let body = o
        .deck
        .shapes(0)
        .unwrap()
        .into_iter()
        .find(|s| s.paragraphs.len() == 2)
        .expect("a two-paragraph body");
    assert!(ops::write_location(&mut o.deck, &location(&body), "A\nB\nC").unwrap());
    let saved = ops::finish_edit(&o, &l, &ops::Check::default()).unwrap();
    let deck = Deck::open(&saved, &l).unwrap();
    let body = deck
        .shapes(0)
        .unwrap()
        .into_iter()
        .find(|s| s.paragraphs.len() == 2)
        .expect("still two paragraphs");
    assert_eq!(body.paragraphs[0].text, "A");
    assert_eq!(body.paragraphs[1].text, "B\nC");
}

/// Finding 2: keys are applied in one pass over the original text.
#[tokio::test]
async fn replace_placeholders_is_one_pass_leftmost_longest_over_the_original() {
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, "a b abc", Rect::inches(1.0, 1.0, 6.0, 1.0))
        .unwrap();
    let t = d
        .add_table(s, 1, 1, Rect::inches(1.0, 3.0, 4.0, 1.0))
        .unwrap();
    d.set_table_cell(s, t, 0, 0, "b a").unwrap();
    rig.host.put(
        "c.pptx",
        Deck::open(&d.save().unwrap(), &test_limits())
            .unwrap()
            .save()
            .unwrap(),
    );
    // `a` becomes `b` and `b` becomes `c`: the `b` that `a` produced is not
    // replaced again. `abc` is longer than `a` at the same place: it wins.
    let result = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "c.pptx", "output_file_name": "o.pptx",
                   "replacements": {"a": "b", "b": "c", "abc": "XYZ", "zzz": "never"}}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    // Matches in the ORIGINAL: text frame `a`, `b`, `abc`; cell `b`, `a`.
    assert_eq!(result["replacements_applied"], 5, "{result}");
    assert_eq!(result["keys_not_found"], json!(["zzz"]));
    let facts =
        elitea_office_core::facts_of_package(&rig.host.get("o.pptx"), &test_limits()).unwrap();
    assert!(
        facts[0].shapes.iter().any(|s| s.text == "b c XYZ"),
        "{:?}",
        facts[0].shapes
    );
    assert_eq!(facts[0].tables[0].cells[0][0], "c b");
    // A replacement that contains another key is not searched again.
    let again = rig
        .call(
            "replace_placeholders",
            json!({"file_name": "c.pptx", "output_file_name": "p.pptx",
                   "replacements": {"a": "abc", "abc": "Q"}}),
        )
        .await;
    assert_eq!(again["replacements_applied"], 3, "{again}");
    let facts =
        elitea_office_core::facts_of_package(&rig.host.get("p.pptx"), &test_limits()).unwrap();
    assert!(
        facts[0].shapes.iter().any(|s| s.text == "abc b Q"),
        "{:?}",
        facts[0].shapes
    );
}

/// Finding 3: distinct texts are deduplicated in a set, and the cap is
/// enforced as soon as it is passed.
#[tokio::test]
async fn distinct_placeholders_are_capped_before_the_model_is_asked() {
    let texts = ["x", "y", "x", "z", "y"];
    assert_eq!(
        ops::distinct_texts(texts.into_iter(), 3).unwrap(),
        ["x", "y", "z"]
    );
    assert!(ops::distinct_texts(texts.into_iter(), 2).is_none());
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    for i in 0..2_001 {
        d.add_text(
            s,
            &format!("{{{{k{i}}}}}"),
            Rect::inches(0.1, 0.1, 1.0, 0.1),
        )
        .unwrap();
    }
    rig.host.put("big.pptx", d.save().unwrap());
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "big.pptx", "output_file_name": "o.pptx", "content_description": "x"}),
        )
        .await;
    assert_eq!(result["status"], "error", "{result}");
    assert!(
        result["message"]
            .as_str()
            .unwrap()
            .contains("more than 2000 distinct"),
        "{result}"
    );
    assert!(
        rig.host.model_calls().is_empty(),
        "refused before any model call"
    );
    // The listing counts distinct texts correctly with repeats.
    let listing = rig
        .call("list_placeholders", json!({"file_name": "big.pptx"}))
        .await;
    assert_eq!(listing["unique_texts"], 2_001);
}

/// Findings 4 and 5: a call opens the deck once, reads the file's package
/// without the library, and serialises once.
#[test]
fn an_edit_serialises_once_and_opens_once() {
    let l = test_limits();
    let base = {
        let mut d = Deck::new(&l);
        let s = d.add_slide(SlideLayout::TitleContent).unwrap();
        d.set_title(s, "t").unwrap();
        d.add_bullets(s, &[Bullet::new("one")]).unwrap();
        d.add_table(s, 1, 1, Rect::inches(1.0, 4.0, 3.0, 1.0))
            .unwrap();
        Deck::open(&d.save().unwrap(), &l).unwrap().save().unwrap()
    };
    let calls = [
        (PptxTool::AddSlide, json!({"layout": "blank", "title": "n"})),
        (PptxTool::SetText, json!({"slide": 1, "text": "hello"})),
        (
            PptxTool::SetTableCell,
            json!({"slide": 1, "row": 1, "column": 1, "text": "c"}),
        ),
        (PptxTool::AddBullets, json!({"slide": 1, "items": ["two"]})),
        (
            PptxTool::AddTable,
            json!({"slide": 1, "rows": [["a", "b"]]}),
        ),
    ];
    for (tool, args) in calls {
        elitea_office_core::testing::reset_counters();
        let (said, bytes_out) = ops::run_edit(tool, &args, &base, None, &l)
            .unwrap_or_else(|e| panic!("{}: {e}", tool.name()));
        let (parsed, serialised, read) = elitea_office_core::testing::counters();
        assert!(!said.is_empty() && !bytes_out.is_empty());
        assert_eq!(parsed, 1, "{}: one library parse", tool.name());
        assert!(
            serialised <= 2,
            "{}: {serialised} serialisations",
            tool.name()
        );
        assert_eq!(serialised, 1, "{}: exactly one in practice", tool.name());
        assert!(read <= 2, "{}: {read} package reads", tool.name());
    }
    // replace_placeholders too.
    elitea_office_core::testing::reset_counters();
    let keys: std::collections::BTreeMap<String, String> =
        [("one".to_owned(), "1".to_owned())].into();
    let (n, _, _) = ops::run_replace(&base, &keys, &l).unwrap();
    assert_eq!(n, 1);
    let (parsed, serialised, _) = elitea_office_core::testing::counters();
    assert_eq!((parsed, serialised), (1, 1));
}

/// Finding 6: a newline in new text is a line break, never a raw newline in
/// a run, and no break from the old text is left behind.
#[tokio::test]
async fn new_text_keeps_its_line_breaks_as_break_runs() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", one_box("old"));
    let shapes = |name: &str| {
        let deck = Deck::open(&rig.host.get(name), &test_limits()).unwrap();
        deck.shapes(0).unwrap()
    };
    rig.text(
        "set_text",
        json!({"file_name": "t.pptx", "slide": 1, "shape": 1, "text": "first\nsecond"}),
    )
    .await;
    let runs = runs_of(&rig.host.get("t.pptx"), 0, 0, 0);
    assert_eq!(
        runs,
        [
            (false, "first".into()),
            (true, String::new()),
            (false, "second".into())
        ]
    );
    assert!(runs.iter().all(|(_, t)| !t.contains('\n')));
    // Rewritten as one line: the break and the second run are gone.
    rig.text(
        "set_text",
        json!({"file_name": "t.pptx", "slide": 1, "shape": 1, "text": "single"}),
    )
    .await;
    assert_eq!(shapes("t.pptx")[0].paragraphs[0].runs.len(), 1);
    assert_eq!(shapes("t.pptx")[0].paragraphs[0].text, "single");
    // fill_template content with a newline lands the same way.
    rig.host.put("f.pptx", one_box("{{x}}"));
    rig.host.reply(r#"[{"key": "{{x}}", "content": "l1\nl2"}]"#);
    let filled = rig
        .call("fill_template", json!({"file_name": "f.pptx", "output_file_name": "g.pptx", "content_description": "d"}))
        .await;
    assert_eq!(filled["status"], "success", "{filled}");
    let runs = runs_of(&rig.host.get("g.pptx"), 0, 0, 0);
    assert_eq!(runs.len(), 3, "{runs:?}");
    assert!(runs.iter().all(|(_, t)| !t.contains('\n')));
    assert!(rig.markdown("g.pptx").contains("l1") && rig.markdown("g.pptx").contains("l2"));
}

/// Finding 7: what the model leaves out is asked for again; when that works
/// the result is a plain success.
#[tokio::test]
async fn translation_asks_again_for_what_a_reply_left_out() {
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    for (i, t) in ["red", "green", "blue"].iter().enumerate() {
        d.add_text(
            s,
            t,
            Rect::inches(0.5, 0.5 + f64::from(u8::try_from(i).unwrap()), 3.0, 0.5),
        )
        .unwrap();
    }
    rig.host.put(
        "c.pptx",
        Deck::open(&d.save().unwrap(), &test_limits())
            .unwrap()
            .save()
            .unwrap(),
    );
    rig.host
        .reply(r#"[{"index": 1, "translation": "rojo"}, {"index": 3, "translation": "azul"}]"#);
    rig.host.reply(r#"[{"index": 4, "translation": "verde"}]"#);
    let result = rig
        .call(
            "translate_presentation",
            json!({"file_name": "c.pptx", "output_file_name": "es.pptx", "target_language": "es"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert!(
        result["message"]
            .as_str()
            .unwrap()
            .starts_with("Successfully translated")
    );
    assert!(result.get("untranslated_paragraphs").is_none());
    let md = rig.markdown("es.pptx");
    assert!(
        md.contains("rojo") && md.contains("verde") && md.contains("azul"),
        "{md}"
    );
    let calls = rig.host.model_calls();
    assert_eq!(calls.len(), 2);
    assert!(
        calls[1].prompt.contains("translate the following 1 texts"),
        "{}",
        calls[1].prompt
    );
    assert!(calls[1].prompt.contains("green"));
}

/// Finding 8: a batch takes only its own keys from the model's reply.
#[tokio::test]
async fn a_batch_keeps_only_its_own_keys() {
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s, "{{a}}", Rect::inches(0.5, 0.5, 3.0, 0.5))
        .unwrap();
    d.add_text(s, "{{b}}", Rect::inches(0.5, 1.5, 3.0, 0.5))
        .unwrap();
    rig.host.put(
        "t.pptx",
        Deck::open(&d.save().unwrap(), &test_limits())
            .unwrap()
            .save()
            .unwrap(),
    );
    rig.host.reply(r#"[{"key": "{{a}}", "content": "first"}]"#);
    // The second batch's reply also names `{{a}}` (and a key nobody has).
    rig.host.reply(
        r#"[{"key": "{{b}}", "content": "second"}, {"key": "{{a}}", "content": "HIJACK"}, {"key": "{{zzz}}", "content": "STRAY"}]"#,
    );
    let result = rig
        .call("fill_template", json!({"file_name": "t.pptx", "output_file_name": "o.pptx", "content_description": "d", "batch_size": 1}))
        .await;
    assert_eq!(result["status"], "success", "{result}");
    let md = rig.markdown("o.pptx");
    assert!(md.contains("first") && md.contains("second"), "{md}");
    assert!(!md.contains("HIJACK") && !md.contains("STRAY"), "{md}");
    let parsed = model::parse_fill_reply(
        r#"[{"key": "other", "content": "x"}]"#,
        &["{{a}}".to_owned()],
    );
    assert_eq!(parsed.len(), 1);
    assert_eq!(parsed["{{a}}"], "[Content for: {{a}}]");
}

/// Finding 9: a cell keeps its paragraph structure through translation.
#[tokio::test]
async fn a_translated_cell_keeps_its_paragraphs_and_its_empty_lines() {
    let rig = Rig::new().await;
    let l = test_limits();
    let mut d = Deck::new(&l);
    let s = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_table(s, 1, 1, Rect::inches(1.0, 1.0, 4.0, 1.0))
        .unwrap();
    let mut o = Deck::open(&d.save().unwrap(), &l).unwrap();
    let table = o.facts().unwrap()[0].tables[0].shape;
    o.set_cell_paragraphs(
        0,
        table,
        0,
        0,
        &["one".to_owned(), String::new(), "three".to_owned()],
    )
    .unwrap();
    rig.host.put("t.pptx", o.save().unwrap());
    rig.host
        .reply(r#"[{"index": 1, "translation": "uno"}, {"index": 2, "translation": "tres"}]"#);
    let result = rig
        .call(
            "translate_presentation",
            json!({"file_name": "t.pptx", "output_file_name": "es.pptx", "target_language": "es"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    let facts = elitea_office_core::facts_of_package(&rig.host.get("es.pptx"), &l).unwrap();
    assert_eq!(facts[0].tables[0].cells[0][0], "uno\n\ntres");
    // And a fill into a cell with several lines keeps them as paragraphs.
    let mut o = Deck::open(&rig.host.get("t.pptx"), &l).unwrap();
    o.set_cell_paragraphs(0, table, 0, 0, &["{{x}}".to_owned()])
        .unwrap();
    rig.host.put("f.pptx", o.save().unwrap());
    rig.host.reply(r#"[{"key": "{{x}}", "content": "p1\np2"}]"#);
    rig.call(
        "fill_template",
        json!({"file_name": "f.pptx", "output_file_name": "g.pptx", "content_description": "d"}),
    )
    .await;
    let facts = elitea_office_core::facts_of_package(&rig.host.get("g.pptx"), &l).unwrap();
    assert_eq!(facts[0].tables[0].cells[0][0], "p1\np2");
}

/// Finding 10: where no model is bound (a pipeline node) the two model tools
/// say so in words that name the way round it, and the others are unaffected.
#[tokio::test]
async fn model_tools_say_plainly_when_no_model_is_bound() {
    let rig = Rig::new().await;
    rig.host.put("t.pptx", one_box("{{x}}"));
    rig.host.fail_model(OfficeFailure::NoModel);
    let result = rig
        .call("fill_template", json!({"file_name": "t.pptx", "output_file_name": "o.pptx", "content_description": "d"}))
        .await;
    assert_eq!(result["status"], "error", "{result}");
    let message = result["message"].as_str().unwrap();
    assert!(
        message.contains("needs a chat model") && message.contains("replace_placeholders"),
        "{message}"
    );
    assert!(!rig.host.has("o.pptx"));
    let listing = rig
        .call("list_placeholders", json!({"file_name": "t.pptx"}))
        .await;
    assert_eq!(listing["total_locations"], 1);
}

/// The toolkit editor's tool list is served from elitea-main's argument
/// schemas. The 19 Rust-only tools live in a second pinned document there
/// (the SDK snapshot is regenerated from the SDK); this keeps it equal to
/// what the family serves. `ELITEA_WRITE_RUST_ONLY_TOOLS=1` rewrites it.
#[test]
fn the_ui_catalogue_lists_every_tool_the_family_serves() {
    const PATH: &str = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../services/elitea-main/internal/rustonly/rust_only_toolkit_tools.json"
    );
    let want = super::families::rust_only_catalogue_document();
    if std::env::var("ELITEA_WRITE_RUST_ONLY_TOOLS").as_deref() == Ok("1") {
        std::fs::write(PATH, serde_json::to_string(&want).unwrap() + "\n").unwrap();
    }
    let have: Value = serde_json::from_str(&std::fs::read_to_string(PATH).unwrap()).unwrap();
    assert_eq!(
        have, want,
        "rust_only_toolkit_tools.json is out of date: rerun with ELITEA_WRITE_RUST_ONLY_TOOLS=1"
    );
    // Every tool is either the SDK's or in the document, none in both.
    let in_doc: Vec<&str> = have["types"]["pptx"]
        .as_object()
        .unwrap()
        .keys()
        .map(String::as_str)
        .collect();
    assert_eq!(in_doc.len(), 19);
    for tool in ALL_TOOLS {
        let sdk = matches!(
            tool,
            PptxTool::FillTemplate | PptxTool::TranslatePresentation
        );
        assert_eq!(in_doc.contains(&tool.name()), !sdk, "{}", tool.name());
    }
}

// -- Review round 2 ------------------------------------------------------------

/// Finding 2 and 6: each real cause has its own sentence, and none points at a
/// tool the toolkit may not have.
#[test]
fn every_host_failure_has_its_own_sentence() {
    let cases = [
        (OfficeFailure::NotFound, "no such file in the bucket"),
        (
            OfficeFailure::AccessRefused,
            "refused access to the file store",
        ),
        (OfficeFailure::AlreadyExists, "already exists"),
        (
            OfficeFailure::UnsupportedType,
            "does not accept that type of file",
        ),
        (
            OfficeFailure::InvalidRequest,
            "file name or bucket is not valid",
        ),
        (OfficeFailure::TooLarge, "larger than the platform accepts"),
        (OfficeFailure::BudgetRefused, "model budget is exhausted"),
        (OfficeFailure::NoModel, "needs a chat model"),
        (OfficeFailure::Unreachable, "could not be reached"),
    ];
    let mut seen = std::collections::HashSet::new();
    for (kind, needle) in cases {
        let sentence = host_sentence("do it", &OfficeHostError::new(kind, "test"));
        assert!(sentence.starts_with("Could not do it:"), "{sentence}");
        assert!(sentence.contains(needle), "{kind:?}: {sentence}");
        assert!(!sentence.contains("list_files"), "{kind:?}: {sentence}");
        assert!(
            seen.insert(sentence),
            "{kind:?} shares its sentence with another cause"
        );
    }
    // Only the missing-model cause talks about pipelines.
    for (kind, _) in cases {
        let sentence = host_sentence("x", &OfficeHostError::new(kind, "t"));
        assert_eq!(
            sentence.contains("pipeline"),
            kind == OfficeFailure::NoModel,
            "{sentence}"
        );
    }
}

/// Finding 3: the name a tool reports is a name the next call accepts, with
/// the bucket named separately; the path form works too.
#[tokio::test]
async fn a_reported_name_works_as_the_next_file_name() {
    let rig = Rig::new().await;
    let created = rig
        .text(
            "create_presentation",
            json!({"file_name": "clients/acme/q3.pptx", "title": "Q3"}),
        )
        .await;
    assert!(
        created.starts_with("Created `clients/acme/q3.pptx` in bucket `bucket` ("),
        "{created}"
    );
    // The reported key, the leading-slash form and the `/bucket/key` form all
    // address the same file.
    for name in [
        "clients/acme/q3.pptx",
        "/clients/acme/q3.pptx",
        "/bucket/clients/acme/q3.pptx",
    ] {
        let md = rig.text("to_markdown", json!({"file_name": name})).await;
        assert!(md.contains("Q3"), "{name}: {md}");
    }
    // Another bucket's prefix is not stripped: it is part of the key.
    let other = rig
        .text(
            "to_markdown",
            json!({"file_name": "/other/clients/acme/q3.pptx"}),
        )
        .await;
    assert!(other.contains("no such file"), "{other}");
    // An edit's report names the key, and that key is the next input.
    let edited = rig
        .text(
            "set_notes",
            json!({"file_name": "/bucket/clients/acme/q3.pptx", "slide": 1, "notes": "n"}),
        )
        .await;
    assert!(
        edited.contains("`clients/acme/q3.pptx` in bucket `bucket`"),
        "{edited}"
    );
    let preview = rig
        .call(
            "render_preview",
            json!({"file_name": "clients/acme/q3.pptx"}),
        )
        .await;
    let key = preview["image"].as_str().expect("image key").to_owned();
    assert!(rig.host.has(&key), "{key}");
    assert_eq!(preview["bucket"], "bucket");
}

/// Finding 5: a taken name is refused unless the caller asked to replace it,
/// and the host is told so; outputs that replace by contract say so.
#[tokio::test]
async fn create_presentation_does_not_overwrite_unless_asked() {
    let rig = Rig::new().await;
    rig.text(
        "create_presentation",
        json!({"file_name": "a.pptx", "title": "one"}),
    )
    .await;
    let again = rig
        .text(
            "create_presentation",
            json!({"file_name": "a.pptx", "title": "two"}),
        )
        .await;
    assert!(
        again.contains("already exists") && again.contains("overwrite=true"),
        "{again}"
    );
    assert!(rig.markdown("a.pptx").contains("one") && !rig.markdown("a.pptx").contains("two"));
    rig.text(
        "create_presentation",
        json!({"file_name": "a.pptx", "title": "three", "overwrite": true}),
    )
    .await;
    assert!(rig.markdown("a.pptx").contains("three"));
    rig.text(
        "set_notes",
        json!({"file_name": "a.pptx", "slide": 1, "notes": "n"}),
    )
    .await;
    let writes = rig.host.writes.lock().unwrap().clone();
    // create (new), create (taken), create (asked), then the edit in place.
    assert_eq!(
        writes,
        [
            ("a.pptx".to_owned(), false),
            ("a.pptx".to_owned(), false),
            ("a.pptx".to_owned(), true),
            ("a.pptx".to_owned(), true),
        ]
    );
}

/// Finding 7: outputs with no name of their own go next to the source.
#[tokio::test]
async fn default_outputs_go_next_to_the_source() {
    let rig = Rig::new().await;
    rig.text(
        "create_presentation",
        json!({"file_name": "clients/acme/q3.pptx", "title": "Q3"}),
    )
    .await;
    let preview = rig
        .call(
            "render_preview",
            json!({"file_name": "clients/acme/q3.pptx"}),
        )
        .await;
    assert_eq!(preview["image"], "clients/acme/q3-slide-1.png");
    let pdf = rig
        .text("export_pdf", json!({"file_name": "clients/acme/q3.pptx"}))
        .await;
    assert!(
        pdf.starts_with("Exported `clients/acme/q3.pdf` in bucket"),
        "{pdf}"
    );
    assert!(rig.host.has("clients/acme/q3.pdf") && rig.host.has("clients/acme/q3-slide-1.png"));
    // A source at the bucket's root keeps its outputs there; an explicit name wins.
    rig.text(
        "create_presentation",
        json!({"file_name": "root.pptx", "title": "R"}),
    )
    .await;
    let root = rig
        .call("render_preview", json!({"file_name": "root.pptx"}))
        .await;
    assert_eq!(root["image"], "root-slide-1.png");
    let named = rig
        .call(
            "render_preview",
            json!({"file_name": "root.pptx", "output_file_name": "elsewhere/p.png"}),
        )
        .await;
    assert_eq!(named["image"], "elsewhere/p.png");
}

/// Finding 8: the template's slide images go with the first batch only; the
/// later batches say they were given.
#[tokio::test]
async fn slide_images_are_sent_with_the_first_batch_only() {
    let rig = Rig::new().await;
    let mut d = Deck::new(&test_limits());
    for text in ["{{a}}", "{{b}}", "{{c}}"] {
        let s = d.add_slide(SlideLayout::Blank).unwrap();
        d.add_text(s, text, Rect::inches(1.0, 1.0, 3.0, 1.0))
            .unwrap();
    }
    rig.host.put(
        "t.pptx",
        Deck::open(&d.save().unwrap(), &test_limits())
            .unwrap()
            .save()
            .unwrap(),
    );
    for key in ["{{a}}", "{{b}}", "{{c}}"] {
        rig.host
            .reply(&json!([{"key": key, "content": "x"}]).to_string());
    }
    let result = rig
        .call(
            "fill_template",
            json!({"file_name": "t.pptx", "output_file_name": "o.pptx",
               "content_description": "d", "batch_size": 1, "pdf_file_name": "t.pdf"}),
        )
        .await;
    assert_eq!(result["status"], "success", "{result}");
    assert_eq!(result["stats"]["visual_context_slides"], 3);
    let calls = rig.host.model_calls();
    let counts: Vec<usize> = calls.iter().map(|c| c.images_png.len()).collect();
    assert_eq!(counts, [3, 0, 0], "images ride with the first batch only");
    assert!(!calls[0].prompt.contains("given with the first batch"));
    for later in &calls[1..] {
        assert!(
            later
                .prompt
                .contains("images of the template's slides were given with the first batch")
        );
    }
    // Without a named PDF no batch has images and none has the note.
    rig.host.replies.lock().unwrap().clear();
    rig.host.requests.lock().unwrap().clear();
    for key in ["{{a}}", "{{b}}", "{{c}}"] {
        rig.host
            .reply(&json!([{"key": key, "content": "x"}]).to_string());
    }
    rig.call(
        "fill_template",
        json!({"file_name": "t.pptx", "output_file_name": "p.pptx",
             "content_description": "d", "batch_size": 1}),
    )
    .await;
    assert!(
        rig.host
            .model_calls()
            .iter()
            .all(|c| c.images_png.is_empty() && !c.prompt.contains("given with the first batch"))
    );
}

#[tokio::test]
async fn a_null_table_cell_is_an_empty_cell_not_the_word_null() {
    let rig = Rig::new().await;
    rig.text(
        "create_presentation",
        json!({"file_name": "t.pptx", "title": "T"}),
    )
    .await;
    rig.text(
        "add_table",
        json!({"file_name": "t.pptx", "slide": 1, "rows": [["h", null], [3, false]]}),
    )
    .await;
    let md = rig.markdown("t.pptx");
    assert!(!md.contains("null"), "{md}");
    assert!(md.contains('3') && md.contains("false"), "{md}");
}

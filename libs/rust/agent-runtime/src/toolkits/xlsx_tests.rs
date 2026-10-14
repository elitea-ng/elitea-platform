//! The `xlsx` family (ADR-0032 P4): the contract, the selected-tools rule,
//! every tool end to end on real packages through an in-memory host, and the
//! rules every write must keep (all or nothing, checked on the saved bytes at
//! the cells aimed at, one sentence per cause, names that round-trip, no
//! overwrite unless asked, one parse and one save per edit).

use std::sync::Arc;

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use elitea_office_core::testing::{counters, reset_counters};
use elitea_office_core::{CellValue, Limits, Workbook, XlsxFacts, XlsxQuery};
use serde_json::{Value, json};

use super::families::artifact::ArtifactToolAuthority;
use super::families::office_common::OfficeConfigErrorCode;
use super::families::office_host::{OfficeFailure, SharedOfficeToolHost};
use super::families::xlsx::config::XlsxToolkitConfig;
use super::families::xlsx::ops;
use super::families::xlsx::schema::{ALL_TOOLS, XlsxTool};
use super::families::xlsx::tools::{build_with_host, build_xlsx_toolset};
use super::materialize::materialize_configured_toolsets_with_artifact_authority;
use super::office_test_support::{
    FailHost, FakeHost, NoPlatform, context, policy, settings, test_limits,
};
use super::sdk_conformance;
use super::snapshot::FrozenToolSnapshot;
use crate::host::PlatformWriter;

/// A real workbook as openpyxl writes it: its text is inline strings.
const INLINE_XLSX: &[u8] = include_bytes!(
    "../../../../../services/elitea-main/internal/infra/extract/testdata/sheet.xlsx"
);

async fn tools_with(
    host: &Arc<FakeHost>,
    selected: Option<Value>,
    limits: Limits,
) -> Vec<Arc<dyn Tool>> {
    let config = XlsxToolkitConfig::parse(&settings(selected)).expect("config");
    let shared: SharedOfficeToolHost = Arc::clone(host) as SharedOfficeToolHost;
    let toolset = build_with_host("book", &config, &policy(), &shared, limits).expect("toolset");
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
        Self::with_limits(test_limits()).await
    }

    async fn with_limits(limits: Limits) -> Self {
        let host = Arc::new(FakeHost::default());
        let tools = tools_with(&host, None, limits).await;
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

    fn facts(&self, name: &str, cells: &[(usize, u32, u16)]) -> XlsxFacts {
        elitea_office_core::facts_of_xlsx_package(
            &self.host.get(name),
            &test_limits(),
            &XlsxQuery {
                cells,
                ranges: &[],
                sheet_mention: None,
                for_delete: false,
            },
        )
        .expect("a valid workbook")
    }

    fn writes(&self) -> usize {
        self.host.writes.lock().expect("writes").len()
    }
}

/// A workbook with a header row, numbers and a formula.
fn sales() -> Vec<u8> {
    let l = test_limits();
    let mut w = Workbook::new(&l);
    w.write_cells(
        0,
        &[
            (0, 0, CellValue::Text("Region".into())),
            (0, 1, CellValue::Text("Amount".into())),
            (1, 0, CellValue::Text("North".into())),
            (1, 1, CellValue::Number(120.0)),
            (2, 0, CellValue::Text("South".into())),
            (2, 1, CellValue::Number(80.5)),
            (
                3,
                1,
                CellValue::Formula {
                    formula: "SUM(B2:B3)".into(),
                    cached: None,
                },
            ),
        ],
    )
    .unwrap();
    Workbook::open(&w.save().unwrap(), &l)
        .unwrap()
        .save()
        .unwrap()
}

// -- The contract ---------------------------------------------------------

#[tokio::test]
async fn the_family_serves_its_10_tools() {
    let rig = Rig::new().await;
    let names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
    assert_eq!(
        names,
        [
            "create_workbook",
            "list_sheets",
            "add_sheet",
            "rename_sheet",
            "delete_sheet",
            "read_range",
            "sheet_to_markdown",
            "write_cells",
            "clear_range",
            "add_chart",
        ]
    );
    for tool in &rig.tools {
        assert!(
            tool.description().starts_with("Toolkit: book\n"),
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
    for read in ["list_sheets", "read_range", "sheet_to_markdown"] {
        assert!(by_name(read).is_read_only(), "{read}");
    }
    assert!(!by_name("write_cells").is_read_only());
}

/// The SDK has no `xlsx` or `excel` toolkit (its `pandas` toolkit writes
/// `.xlsx` through `df.to_excel` and is not superseded). The day the SDK grows
/// one, its names and schemas are the contract, so this fails and the family is
/// reconciled with them.
#[test]
fn there_is_no_sdk_xlsx_toolkit_to_stay_callable_by() {
    for name in ["xlsx", "excel"] {
        assert!(!sdk_conformance::has_type(name), "{name}");
    }
}

#[tokio::test]
async fn the_selected_tools_rule_follows_the_artifact_family() {
    let host = Arc::new(FakeHost::default());
    for (selected, want) in [
        (None, 10),
        (Some(json!([])), 10),
        (Some(Value::Null), 10),
        (Some(json!(["write_cells", "read_range", "nope"])), 2),
        (Some(json!(["nope"])), 0),
    ] {
        let tools = tools_with(&host, selected.clone(), test_limits()).await;
        assert_eq!(tools.len(), want, "{selected:?}");
    }
    let err = XlsxToolkitConfig::parse(json!({"bucket_name": "x"}).as_object().unwrap())
        .err()
        .expect("a one-letter bucket");
    assert_eq!(err.code(), OfficeConfigErrorCode::InvalidConfiguration);
    assert_eq!(err.to_string(), "the xlsx toolkit configuration is invalid");
}

#[test]
fn the_ui_catalogue_lists_the_xlsx_type_and_every_tool() {
    let entry = super::families::xlsx::schema::type_entry();
    let tools = entry["args_schemas"].as_object().unwrap();
    assert_eq!(tools.len(), ALL_TOOLS.len());
    for tool in ALL_TOOLS {
        let schema = &tools[tool.name()];
        assert_eq!(schema["type"], "object");
        assert!(schema["title"].as_str().unwrap().ends_with("Args"));
        assert!(schema.get("additionalProperties").is_none());
    }
    // A property that is a union is an anyOf, as the SDK's pydantic schemas are.
    assert!(tools["write_cells"]["properties"]["sheet"]["anyOf"].is_array());
    assert_eq!(entry["metadata"]["label"], "XLSX");
    assert_eq!(entry["settings"]["required"], json!(["bucket_name"]));
}

#[test]
fn the_rust_only_document_carries_pptx_tools_and_both_new_types() {
    let doc = super::families::rust_only_catalogue_document();
    assert_eq!(doc["schema_version"], "elitea.rust-only-toolkit-tools.v1");
    assert_eq!(doc["types"]["pptx"].as_object().unwrap().len(), 19);
    assert_eq!(
        doc["new_types"]["docx"]["args_schemas"]
            .as_object()
            .unwrap()
            .len(),
        13
    );
    assert_eq!(
        doc["new_types"]["xlsx"]["args_schemas"]
            .as_object()
            .unwrap()
            .len(),
        10
    );
}

/// The toolkit editor's tool list is served from elitea-main's second pinned
/// document (the SDK snapshot is regenerated from the SDK and has no `docx` or
/// `xlsx`); this keeps it equal to what the families serve.
/// `ELITEA_WRITE_RUST_ONLY_TOOLS=1` rewrites it.
#[test]
fn the_ui_catalogue_lists_every_tool_the_families_serve() {
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
}

// -- create ---------------------------------------------------------------

#[tokio::test]
async fn create_workbook_makes_a_workbook_and_never_replaces_unasked() {
    let rig = Rig::new().await;
    let said = rig
        .text(
            "create_workbook",
            json!({"file_name": "books/a.xlsx", "sheet_name": "Budget"}),
        )
        .await;
    assert!(said.contains("`books/a.xlsx` in bucket `bucket`"), "{said}");
    let names: Vec<String> = rig
        .facts("books/a.xlsx", &[])
        .sheets
        .iter()
        .map(|s| s.name.clone())
        .collect();
    assert_eq!(names, ["Budget"]);
    let before = rig.host.get("books/a.xlsx");
    let said = rig
        .text("create_workbook", json!({"file_name": "books/a.xlsx"}))
        .await;
    assert!(
        said.contains("already exists") && said.contains("overwrite=true"),
        "{said}"
    );
    assert_eq!(rig.host.get("books/a.xlsx"), before);
    assert_eq!(
        rig.host.writes.lock().unwrap().as_slice(),
        [
            ("books/a.xlsx".to_owned(), false),
            ("books/a.xlsx".to_owned(), false)
        ]
    );
    rig.text(
        "create_workbook",
        json!({"file_name": "books/a.xlsx", "overwrite": true}),
    )
    .await;
    assert_eq!(rig.facts("books/a.xlsx", &[]).sheets[0].name, "Sheet1");
    // A bad sheet name is refused before anything is written.
    let writes = rig.writes();
    let said = rig
        .text(
            "create_workbook",
            json!({"file_name": "b.xlsx", "sheet_name": "bad/name"}),
        )
        .await;
    assert!(said.contains("may not contain"), "{said}");
    assert_eq!(rig.writes(), writes);
}

// -- write_cells --------------------------------------------------------------

#[tokio::test]
async fn write_cells_stores_typed_values_and_formulas_and_checks_them_in_the_file() {
    let rig = Rig::new().await;
    rig.text("create_workbook", json!({"file_name": "b.xlsx"}))
        .await;
    let said = rig
        .text(
            "write_cells",
            json!({"file_name": "b.xlsx",
            "start_cell": "A1",
            "values": [["Name", "Qty", "Ok"], ["12", 12, true]],
            "cells": [
                {"cell": "D2", "formula": "=SUM(B2:B2)"},
                {"cell": "E1", "value": "later"},
                {"cell": "E1", "value": "last wins"}
            ]}),
        )
        .await;
    assert!(said.contains("Wrote 8 cells in sheet `Sheet1`"), "{said}");
    let want = [
        (0, 0, 0),
        (0, 1, 0),
        (0, 1, 1),
        (0, 1, 2),
        (0, 1, 3),
        (0, 0, 4),
    ];
    let f = rig.facts("b.xlsx", &want);
    // A string stays text even when it looks like a number.
    assert_eq!(f.cells[&(0, 1, 0)], CellValue::Text("12".into()));
    assert_eq!(f.cells[&(0, 1, 1)], CellValue::Number(12.0));
    assert_eq!(f.cells[&(0, 1, 2)], CellValue::Bool(true));
    match &f.cells[&(0, 1, 3)] {
        CellValue::Formula { formula, .. } => assert_eq!(formula, "SUM(B2:B2)"),
        other => panic!("{other:?}"),
    }
    assert_eq!(f.cells[&(0, 0, 4)], CellValue::Text("last wins".into()));
    assert_eq!(f.sheets[0].value_cells, 8);
    // null clears.
    rig.text(
        "write_cells",
        json!({"file_name": "b.xlsx", "cells": [{"cell": "E1", "value": null}]}),
    )
    .await;
    let f = rig.facts("b.xlsx", &[(0, 0, 4)]);
    assert!(f.cells.is_empty());
    assert_eq!(f.sheets[0].value_cells, 7);
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn every_refusal_happens_before_anything_is_written() {
    let rig = Rig::new().await;
    rig.host.put("b.xlsx", sales());
    let before = rig.host.get("b.xlsx");
    let cases = [
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "A9", "value": "ok"}, {"cell": "ZZZZ1", "value": 1}]}),
            "not a cell address",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "A9", "value": "ok"}, {"cell": "B9"}]}),
            "neither a value nor a formula",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "B9", "value": 1, "formula": "A1"}]}),
            "both a value and a formula",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "B9", "value": ""}]}),
            "empty string",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "B9", "value": {"a": 1}}]}),
            "string, a number",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "values": [[1]]}),
            "start_cell",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "start_cell": "XFD1", "values": [[1, 2]]}),
            "edge of the sheet",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "sheet": "Nope", "cells": [{"cell": "A1", "value": 1}]}),
            "no sheet named `Nope`",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx", "sheet": 4, "cells": [{"cell": "A1", "value": 1}]}),
            "Sheet 4 does not exist",
        ),
        (
            "write_cells",
            json!({"file_name": "b.xlsx"}),
            "Give `cells`",
        ),
        (
            "clear_range",
            json!({"file_name": "b.xlsx", "range": "A1:"}),
            "not a range",
        ),
        (
            "add_sheet",
            json!({"file_name": "b.xlsx", "name": "sheet1"}),
            "already exists",
        ),
        (
            "add_sheet",
            json!({"file_name": "b.xlsx", "name": "a/b"}),
            "may not contain",
        ),
        (
            "add_sheet",
            json!({"file_name": "b.xlsx", "name": "x".repeat(32)}),
            "31 characters",
        ),
        (
            "delete_sheet",
            json!({"file_name": "b.xlsx", "sheet": 1}),
            "the last one cannot be deleted",
        ),
        (
            "add_chart",
            json!({"file_name": "b.xlsx", "series": [{"values": "not a range"}]}),
            "not a range",
        ),
        (
            "add_chart",
            json!({"file_name": "b.xlsx", "series": [{"values": "Elsewhere!B2:B3"}]}),
            "sheet that does not exist",
        ),
        (
            "add_chart",
            json!({"file_name": "b.xlsx", "chart_type": "radar", "series": [{"values": "B2:B3"}]}),
            "chart_type `radar` is not available",
        ),
    ];
    for (tool, args, want) in cases {
        let said = rig.text(tool, args.clone()).await;
        assert!(said.contains(want), "{tool} {args}: {said}");
    }
    assert_eq!(rig.host.get("b.xlsx"), before);
    assert_eq!(rig.writes(), 0, "a refused call wrote a file");
}

#[tokio::test]
async fn a_batch_counts_distinct_cells_against_the_cell_cap_and_is_refused_whole() {
    let limits = Limits {
        max_cells: 5,
        ..test_limits()
    };
    let rig = Rig::with_limits(limits).await;
    rig.host.put("b.xlsx", {
        let mut w = Workbook::new(&test_limits());
        w.write_cells(
            0,
            &[
                (0, 0, CellValue::Number(1.0)),
                (0, 1, CellValue::Number(2.0)),
            ],
        )
        .unwrap();
        w.save().unwrap()
    });
    // The same cell four times is one cell: 2 + 3 distinct = 5, within the cap.
    let said = rig
        .text(
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [
                {"cell": "C1", "value": 1}, {"cell": "C1", "value": 2},
                {"cell": "C1", "value": 3}, {"cell": "C1", "value": 4},
                {"cell": "D1", "value": 1}, {"cell": "E1", "value": 1},
                {"cell": "A1", "value": 9}
            ]}),
        )
        .await;
    assert!(said.contains("Wrote 4 cells"), "{said}");
    let before = rig.host.get("b.xlsx");
    let writes = rig.writes();
    // One more distinct cell passes the cap: the whole batch is refused,
    // including the overwrite in it.
    let said = rig
        .text(
            "write_cells",
            json!({"file_name": "b.xlsx", "cells": [{"cell": "A1", "value": 7}, {"cell": "F1", "value": 1}]}),
        )
        .await;
    assert!(
        said.contains("safety limit") && said.contains("cells"),
        "{said}"
    );
    assert_eq!(rig.host.get("b.xlsx"), before);
    assert_eq!(rig.writes(), writes);
}

// -- clear, sheets, charts ---------------------------------------------------------

#[tokio::test]
async fn clear_range_clears_the_range_and_nothing_else() {
    let rig = Rig::new().await;
    rig.host.put("b.xlsx", sales());
    let said = rig
        .text(
            "clear_range",
            json!({"file_name": "b.xlsx", "range": "A2:B3"}),
        )
        .await;
    assert!(
        said.contains("Cleared 4 cells with a value in A2:B3"),
        "{said}"
    );
    let f = rig.facts("b.xlsx", &[(0, 0, 0), (0, 1, 0), (0, 3, 1)]);
    assert_eq!(
        f.cells.len(),
        2,
        "the header and the formula stay: {:?}",
        f.cells
    );
    assert_eq!(f.sheets[0].value_cells, 3);
    // A range with nothing in it clears nothing and still saves a valid file.
    let said = rig
        .text(
            "clear_range",
            json!({"file_name": "b.xlsx", "range": "H1:H5"}),
        )
        .await;
    assert!(said.contains("Cleared 0 cells"), "{said}");
}

#[tokio::test]
async fn sheets_are_added_renamed_and_deleted_and_the_rest_is_checked() {
    let rig = Rig::new().await;
    rig.host.put("b.xlsx", sales());
    rig.text("add_sheet", json!({"file_name": "b.xlsx", "name": "Notes"}))
        .await;
    rig.text(
        "write_cells",
        json!({"file_name": "b.xlsx", "sheet": "Notes", "cells": [{"cell": "A1", "value": "hi"}]}),
    )
    .await;
    let said = rig
        .text(
            "rename_sheet",
            json!({"file_name": "b.xlsx", "sheet": 2, "new_name": "Memo"}),
        )
        .await;
    assert!(said.contains("Renamed sheet 2 `Notes` to `Memo`"), "{said}");
    let names = |rig: &Rig| -> Vec<String> {
        rig.facts("b.xlsx", &[])
            .sheets
            .iter()
            .map(|s| s.name.clone())
            .collect()
    };
    assert_eq!(names(&rig), ["Sheet1", "Memo"]);
    let said = rig
        .text(
            "delete_sheet",
            json!({"file_name": "b.xlsx", "sheet": "memo"}),
        )
        .await;
    assert!(said.contains("Deleted sheet `Memo`"), "{said}");
    assert_eq!(names(&rig), ["Sheet1"]);
    // Sheet1's cells are untouched through all of it.
    assert_eq!(rig.facts("b.xlsx", &[]).sheets[0].value_cells, 7);
}

#[tokio::test]
async fn a_sheet_that_a_formula_or_chart_names_is_not_renamed_or_deleted() {
    let rig = Rig::new().await;
    let l = test_limits();
    let mut w = Workbook::new(&l);
    w.add_sheet("Data").unwrap();
    w.write_cells(1, &[(0, 0, CellValue::Number(5.0))]).unwrap();
    w.write_cells(
        0,
        &[(
            0,
            0,
            CellValue::Formula {
                formula: "Data!A1*2".into(),
                cached: None,
            },
        )],
    )
    .unwrap();
    rig.host.put("b.xlsx", w.save().unwrap());
    let before = rig.host.get("b.xlsx");
    for tool in [
        json!({"sheet": "Data", "new_name": "Other"}),
        json!({"sheet": "Data"}),
    ] {
        let name = if tool.get("new_name").is_some() {
            "rename_sheet"
        } else {
            "delete_sheet"
        };
        let mut args = tool;
        args["file_name"] = json!("b.xlsx");
        let said = rig.text(name, args).await;
        assert!(said.contains("referred to by name"), "{name}: {said}");
    }
    assert_eq!(rig.host.get("b.xlsx"), before);
    assert_eq!(rig.writes(), 0);
}

#[tokio::test]
async fn add_chart_is_in_the_file_on_the_sheet_with_its_ranges() {
    let rig = Rig::new().await;
    rig.host.put("b.xlsx", sales());
    let said = rig
        .text(
            "add_chart",
            json!({"file_name": "b.xlsx", "chart_type": "bar", "title": "Sales",
                   "series": [{"name": "Amount", "values": "B2:B3"}],
                   "categories": "A2:A3"}),
        )
        .await;
    assert!(said.contains("Added a chart to sheet `Sheet1`"), "{said}");
    let f = rig.facts("b.xlsx", &[]);
    let chart = &f.sheets[0].charts[0];
    assert_eq!(chart.kind, "bar");
    assert_eq!(chart.title.as_deref(), Some("Sales"));
    assert!(
        chart.refs.iter().any(|r| r == "'Sheet1'!$B$2:$B$3"),
        "{:?}",
        chart.refs
    );
    // The chart survives a later edit (it is kept, and counted).
    rig.text(
        "write_cells",
        json!({"file_name": "b.xlsx", "cells": [{"cell": "C9", "value": 1}]}),
    )
    .await;
    assert_eq!(rig.facts("b.xlsx", &[]).sheets[0].charts.len(), 1);
}

// -- reads -------------------------------------------------------------------

#[tokio::test]
async fn list_sheets_and_reads_show_values_formulas_and_addresses() {
    let rig = Rig::new().await;
    rig.host.put("b.xlsx", sales());
    let listing = rig
        .call("list_sheets", json!({"file_name": "b.xlsx"}))
        .await;
    assert_eq!(listing["sheets"][0]["name"], "Sheet1");
    assert_eq!(listing["sheets"][0]["used_range"], "A1:B4");
    assert_eq!(listing["sheets"][0]["cells_with_a_value"], 7);
    let md = rig
        .text(
            "read_range",
            json!({"file_name": "b.xlsx", "range": "A1:B4"}),
        )
        .await;
    assert!(md.contains("| | A | B |"), "{md}");
    assert!(md.contains("| 2 | North | 120 |"), "{md}");
    assert!(md.contains("| 3 | South | 80.5 |"), "{md}");
    assert!(md.contains("- B4: =SUM(B2:B3)"), "{md}");
    let json_out = rig
        .call(
            "read_range",
            json!({"file_name": "b.xlsx", "range": "A2:B3", "format": "json"}),
        )
        .await;
    assert_eq!(json_out["rows"], json!([["North", 120.0], ["South", 80.5]]));
    assert_eq!(json_out["range"], "A2:B3");
    // The whole sheet, and a sheet with nothing in it.
    let whole = rig
        .text("sheet_to_markdown", json!({"file_name": "b.xlsx"}))
        .await;
    assert!(whole.contains("A1:B4"), "{whole}");
    rig.text("add_sheet", json!({"file_name": "b.xlsx", "name": "Blank"}))
        .await;
    let empty = rig
        .text(
            "sheet_to_markdown",
            json!({"file_name": "b.xlsx", "sheet": "Blank"}),
        )
        .await;
    assert!(empty.contains("is empty"), "{empty}");
}

#[tokio::test]
async fn a_long_sheet_is_cut_at_the_cell_cap_and_says_so() {
    let rig = Rig::new().await;
    let l = test_limits();
    let mut w = Workbook::new(&l);
    let rows: Vec<(u32, u16, CellValue)> = (0..3_000u32)
        .flat_map(|r| (0..10u16).map(move |c| (r, c, CellValue::Number(f64::from(r)))))
        .collect();
    for chunk in rows.chunks(10_000) {
        w.write_cells(0, chunk).unwrap();
    }
    rig.host.put("big.xlsx", w.save().unwrap());
    let md = rig
        .text("sheet_to_markdown", json!({"file_name": "big.xlsx"}))
        .await;
    assert!(md.contains("Showing the first 2000 of 3000 rows"), "{md}");
    assert!(md.contains("starting at row 2001"), "{md}");
    assert!(md.contains("| 2000 |"), "{md}");
    assert!(!md.contains("| 2001 |"), "{md}");
    assert_eq!(rig.writes(), 0);
}

#[tokio::test]
async fn inline_strings_are_read_but_an_edit_that_would_erase_them_is_refused() {
    let rig = Rig::new().await;
    rig.host.put("inline.xlsx", INLINE_XLSX.to_vec());
    // The package reader sees the text the workbook library reads as empty.
    let md = rig
        .text("read_range", json!({"file_name": "inline.xlsx"}))
        .await;
    assert!(
        md.contains("north") && md.contains("FIXTURETOKENXLSX"),
        "{md}"
    );
    assert!(md.contains("| 2 | north | 120 |"), "{md}");
    let before = rig.host.get("inline.xlsx");
    for (tool, args) in [
        (
            "write_cells",
            json!({"cells": [{"cell": "D1", "value": 1}]}),
        ),
        ("clear_range", json!({"range": "A1:B3"})),
        ("add_sheet", json!({"name": "New"})),
    ] {
        let mut args = args;
        args["file_name"] = json!("inline.xlsx");
        let said = rig.text(tool, args).await;
        assert!(said.contains("inline strings"), "{tool}: {said}");
        assert!(said.contains("Nothing was saved"), "{tool}: {said}");
    }
    assert_eq!(rig.host.get("inline.xlsx"), before);
    assert_eq!(rig.writes(), 0);
}

// -- names ---------------------------------------------------------------------

#[tokio::test]
async fn a_reported_name_works_as_the_next_file_name_and_output_leaves_the_source() {
    let rig = Rig::new().await;
    rig.text(
        "create_workbook",
        json!({"file_name": "/bucket/finance/q3.xlsx"}),
    )
    .await;
    assert!(rig.host.has("finance/q3.xlsx"));
    let said = rig
        .text(
            "write_cells",
            json!({"file_name": "/bucket/finance/q3.xlsx",
                   "cells": [{"cell": "A1", "value": "x"}],
                   "output_file_name": "finance/q3-copy.xlsx"}),
        )
        .await;
    assert!(
        said.contains("`finance/q3-copy.xlsx` in bucket `bucket`"),
        "{said}"
    );
    assert_eq!(rig.facts("finance/q3.xlsx", &[]).sheets[0].value_cells, 0);
    assert_eq!(
        rig.facts("finance/q3-copy.xlsx", &[]).sheets[0].value_cells,
        1
    );
    // The key just reported chains.
    let said = rig
        .text(
            "write_cells",
            json!({"file_name": "finance/q3-copy.xlsx", "cells": [{"cell": "A2", "value": "y"}]}),
        )
        .await;
    assert!(said.contains("Saved `finance/q3-copy.xlsx`"), "{said}");
}

// -- host failures --------------------------------------------------------------

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
        let config = XlsxToolkitConfig::parse(&settings(None)).unwrap();
        let host: SharedOfficeToolHost = Arc::new(FailHost(kind));
        let toolset = build_with_host("book", &config, &policy(), &host, test_limits()).unwrap();
        let tools = toolset
            .tools(context() as Arc<dyn ReadonlyContext>)
            .await
            .unwrap();
        let tool = tools.iter().find(|t| t.name() == "list_sheets").unwrap();
        let said = tool
            .execute(
                context() as Arc<dyn ToolContext>,
                json!({"file_name": "x.xlsx"}),
            )
            .await
            .unwrap();
        let said = said.as_str().expect("a sentence").to_owned();
        assert!(
            said.starts_with("Could not read `x.xlsx`"),
            "{kind:?}: {said}"
        );
        assert!(!said.contains("list_files"), "{said}");
        assert!(sentences.insert(said.clone()), "{kind:?} repeats: {said}");
    }
    assert_eq!(sentences.len(), 7);
    assert!(
        sentences
            .iter()
            .any(|s| s.contains("does not accept that type") && s.contains(".xlsx"))
    );
}

// -- one parse, one save ---------------------------------------------------------

#[test]
fn an_edit_opens_once_and_serialises_once() {
    let l = test_limits();
    let bytes = sales();
    for (tool, args) in [
        (
            XlsxTool::WriteCells,
            json!({"cells": [{"cell": "D1", "value": "x"}, {"cell": "A1", "value": null}]}),
        ),
        (XlsxTool::ClearRange, json!({"range": "A1:B2"})),
        (XlsxTool::AddSheet, json!({"name": "More"})),
        (
            XlsxTool::RenameSheet,
            json!({"sheet": 1, "new_name": "Renamed"}),
        ),
        (
            XlsxTool::AddChart,
            json!({"series": [{"values": "B2:B3"}], "categories": "A2:A3"}),
        ),
    ] {
        reset_counters();
        ops::run_edit(tool, &args, &bytes, &l).unwrap_or_else(|e| panic!("{tool:?}: {e}"));
        let (opens, saves, reads) = counters();
        assert_eq!(opens, 1, "{tool:?}: library parses");
        assert_eq!(saves, 1, "{tool:?}: serialisations");
        assert_eq!(reads, 2, "{tool:?}: package reads (before and after)");
    }
    // A read parses nothing with the library, and validates and scans the
    // package ONCE (the sheet is picked and the range collected in one pass).
    for args in [
        json!({}),
        json!({"range": "A1:B2"}),
        json!({"sheet": "Sheet1", "range": "A2:A3"}),
    ] {
        reset_counters();
        ops::read_range(&bytes, &args, &l, false).unwrap();
        assert_eq!(counters(), (0, 0, 1), "{args}");
    }
    reset_counters();
    ops::read_range(&bytes, &json!({}), &l, true).unwrap();
    assert_eq!(counters(), (0, 0, 1));
    reset_counters();
    ops::list_sheets(&bytes, &l).unwrap();
    assert_eq!(counters(), (0, 0, 1));
}

// -- verification at the target ---------------------------------------------------

#[test]
fn the_saved_bytes_are_judged_cell_by_cell_not_workbook_wide() {
    // A workbook-wide search for the written text would be satisfied by the
    // same text anywhere. The plan is checked at the cell it was aimed at.
    let l = test_limits();
    let bytes = sales();
    let args = json!({"cells": [{"cell": "D1", "value": "needle"}]});
    let (_, saved) = ops::run_edit(XlsxTool::WriteCells, &args, &bytes, &l).unwrap();
    let f = elitea_office_core::facts_of_xlsx_package(
        &saved,
        &l,
        &XlsxQuery {
            cells: &[(0, 0, 3), (0, 0, 4)],
            ranges: &[],
            sheet_mention: None,
            for_delete: false,
        },
    )
    .unwrap();
    assert_eq!(f.cells[&(0, 0, 3)], CellValue::Text("needle".into()));
    assert!(!f.cells.contains_key(&(0, 0, 4)));
}

// -- materialization ---------------------------------------------------------------

fn xlsx_version() -> Value {
    json!({"tools": [{
        "id": 7,
        "type": "xlsx",
        "toolkit_name": "book",
        "settings": {"bucket_name": "bucket", "selected_tools": ["read_range", "write_cells"]}
    }]})
}

#[tokio::test]
async fn materialization_needs_the_office_capability_on_the_artifact_authority() {
    let host = Arc::new(FakeHost::default());
    let platform: Arc<dyn PlatformWriter> = Arc::new(NoPlatform);
    let version = xlsx_version();
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
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
    assert_eq!(names, ["read_range", "write_cells"]);

    let bare = ArtifactToolAuthority::new(platform);
    let config = XlsxToolkitConfig::parse(&settings(None)).expect("config");
    assert!(
        build_xlsx_toolset("book", &config, &policy(), &bare)
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

// -- review round 1 -----------------------------------------------------------------

fn facts_with_sheets(
    sheets: &[&str],
    parts: &[(&str, usize)],
    exempt: &[(&str, usize)],
) -> XlsxFacts {
    use elitea_office_core::XlsxSheet;
    let kinds = |list: &[(&str, usize)]| list.iter().map(|(k, n)| ((*k).to_owned(), *n)).collect();
    XlsxFacts {
        sheets: sheets
            .iter()
            .map(|name| XlsxSheet {
                name: (*name).to_owned(),
                charts: Vec::new(),
                value_cells: 1,
                used_range: None,
            })
            .collect(),
        parts: kinds(parts),
        delete_exempt: kinds(exempt),
        ..XlsxFacts::default()
    }
}

#[test]
fn deleting_a_sheet_exempts_only_what_no_other_root_reaches() {
    let names = vec!["A".to_owned(), "B".to_owned()];
    let plan = ops::Plan::Delete { sheet: 0 };
    // A and B share an image; A alone has a chart; the workbook holds a pivot
    // cache that A's pivot table also reaches.
    let before = facts_with_sheets(
        &["A", "B"],
        &[
            ("xl/media/image", 1),
            ("xl/charts/chart", 1),
            ("xl/worksheets/sheet", 2),
            ("xl/pivotCache/pivotCacheDefinition", 1),
        ],
        &[("xl/charts/chart", 1), ("xl/worksheets/sheet", 1)],
    );
    // Right: A's chart and sheet part are gone; the shared image and the pivot
    // cache stay.
    let right = facts_with_sheets(
        &["B"],
        &[
            ("xl/media/image", 1),
            ("xl/worksheets/sheet", 1),
            ("xl/pivotCache/pivotCacheDefinition", 1),
        ],
        &[],
    );
    ops::verify(&before, &right, &plan, &names).expect("only A's own parts went");
    // The library also dropped the shared image: refused, whatever else.
    let lost_image = facts_with_sheets(
        &["B"],
        &[
            ("xl/worksheets/sheet", 1),
            ("xl/pivotCache/pivotCacheDefinition", 1),
        ],
        &[],
    );
    let said = ops::verify(&before, &lost_image, &plan, &names).expect_err("a shared image");
    assert!(
        said.contains("Nothing was saved") && said.contains("xl/media/image"),
        "{said}"
    );
    // The library dropped a workbook-level part (the pivot cache).
    let lost_cache = facts_with_sheets(
        &["B"],
        &[("xl/media/image", 1), ("xl/worksheets/sheet", 1)],
        &[],
    );
    let said = ops::verify(&before, &lost_cache, &plan, &names).expect_err("a workbook part");
    assert!(said.contains("pivotCacheDefinition"), "{said}");
    // The remaining sheet is whole: its cells and charts are unchanged.
    let mut changed = right.clone();
    changed.sheets[0].value_cells = 0;
    assert!(ops::verify(&before, &changed, &plan, &names).is_err());
}

#[tokio::test]
async fn a_sheet_with_a_chart_of_its_own_can_be_deleted_and_the_rest_is_untouched() {
    let rig = Rig::new().await;
    let l = test_limits();
    let mut w = Workbook::new(&l);
    w.add_sheet("Two").unwrap();
    w.write_cells(0, &[(0, 0, CellValue::Text("keep".into()))])
        .unwrap();
    w.write_cells(
        1,
        &[
            (0, 0, CellValue::Number(1.0)),
            (1, 0, CellValue::Number(2.0)),
        ],
    )
    .unwrap();
    rig.host.put("b.xlsx", w.save().unwrap());
    rig.text(
        "add_chart",
        json!({"file_name": "b.xlsx", "sheet": "Two", "series": [{"values": "A1:A2"}]}),
    )
    .await;
    assert_eq!(rig.facts("b.xlsx", &[]).sheets[1].charts.len(), 1);
    let said = rig
        .text(
            "delete_sheet",
            json!({"file_name": "b.xlsx", "sheet": "Two"}),
        )
        .await;
    assert!(said.contains("Deleted sheet `Two`"), "{said}");
    let f = rig.facts("b.xlsx", &[(0, 0, 0)]);
    assert_eq!(f.sheets.len(), 1);
    assert_eq!(f.cells[&(0, 0, 0)], CellValue::Text("keep".into()));
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn a_sheet_named_anywhere_is_not_renamed_or_deleted_and_scoped_names_follow_the_sheet() {
    use elitea_office_core::testing::replace_in_part;
    let rig = Rig::new().await;
    let l = test_limits();
    let mut base = Workbook::new(&l);
    base.add_sheet("Data").unwrap();
    base.add_sheet("Last").unwrap();
    let base = base.save().unwrap();
    let cases: [(&str, Vec<u8>); 3] = [
        (
            "a defined name",
            replace_in_part(
                &base,
                "xl/workbook.xml",
                "</sheets>",
                "</sheets><definedNames><definedName name=\"N\">'data'!$A$1</definedName></definedNames>",
            ),
        ),
        (
            "a data validation",
            replace_in_part(
                &base,
                "xl/worksheets/sheet1.xml",
                "</sheetData>",
                "</sheetData><dataValidations count=\"1\"><dataValidation type=\"list\" sqref=\"A1\"><formula1>Data!$A$1:$A$3</formula1></dataValidation></dataValidations>",
            ),
        ),
        (
            "a hyperlink",
            replace_in_part(
                &base,
                "xl/worksheets/sheet1.xml",
                "</sheetData>",
                "</sheetData><hyperlinks><hyperlink ref=\"A1\" location=\"DATA!B2\"/></hyperlinks>",
            ),
        ),
    ];
    for (what, bytes) in cases {
        rig.host.put("b.xlsx", bytes.clone());
        for (tool, args) in [
            (
                "rename_sheet",
                json!({"sheet": "Data", "new_name": "Renamed"}),
            ),
            ("delete_sheet", json!({"sheet": "Data"})),
        ] {
            let mut args = args;
            args["file_name"] = json!("b.xlsx");
            let said = rig.text(tool, args).await;
            assert!(
                said.contains("referred to by name"),
                "{what} {tool}: {said}"
            );
        }
        assert_eq!(rig.host.get("b.xlsx"), bytes);
    }
    // Names scoped to sheets by position no longer block: the autofilter's name
    // on `Data` goes with it on a delete, the print area on `Last` moves down.
    let scoped = replace_in_part(
        &base,
        "xl/workbook.xml",
        "</sheets>",
        "</sheets><definedNames><definedName name=\"_xlnm._FilterDatabase\" localSheetId=\"1\">Data!$A$1:$C$5</definedName><definedName name=\"_xlnm.Print_Area\" localSheetId=\"2\">Last!$A$1:$C$3</definedName></definedNames>",
    );
    let names_of = |rig: &Rig, file: &str| -> Vec<(String, Option<usize>, String)> {
        rig.facts(file, &[])
            .defined_names
            .into_iter()
            .map(|d| (d.name, d.local, d.text))
            .collect()
    };
    rig.host.put("s.xlsx", scoped.clone());
    let said = rig
        .text(
            "rename_sheet",
            json!({"file_name": "s.xlsx", "sheet": "Data", "new_name": "My Data"}),
        )
        .await;
    assert!(said.contains("Renamed sheet 2"), "{said}");
    assert_eq!(
        names_of(&rig, "s.xlsx")[0],
        (
            "_xlnm._FilterDatabase".to_owned(),
            Some(1),
            "'My Data'!$A$1:$C$5".to_owned()
        )
    );
    rig.host.put("s.xlsx", scoped);
    let said = rig
        .text(
            "delete_sheet",
            json!({"file_name": "s.xlsx", "sheet": "Data"}),
        )
        .await;
    assert!(said.contains("Deleted sheet `Data`"), "{said}");
    assert_eq!(
        names_of(&rig, "s.xlsx"),
        [(
            "_xlnm.Print_Area".to_owned(),
            Some(1),
            "Last!$A$1:$C$3".to_owned()
        )]
    );
    // A workbook-scoped name that refers to the sheet still blocks.
    let global = replace_in_part(
        &base,
        "xl/workbook.xml",
        "</sheets>",
        "</sheets><definedNames><definedName name=\"G\">Data!$A$1</definedName></definedNames>",
    );
    rig.host.put("g.xlsx", global.clone());
    let said = rig
        .text(
            "delete_sheet",
            json!({"file_name": "g.xlsx", "sheet": "Data"}),
        )
        .await;
    assert!(said.contains("referred to by name"), "{said}");
    assert_eq!(rig.host.get("g.xlsx"), global);
}
